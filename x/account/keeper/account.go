package keeper

import (
	"context"
	"errors"

	"cosmossdk.io/collections"
	"cosmossdk.io/math"

	sdk "github.com/cosmos/cosmos-sdk/types"

	perptypes "github.com/perpdex/perpdex-l1/types"
	"github.com/perpdex/perpdex-l1/x/account/types"
)

func (k Keeper) GetAccount(ctx context.Context, idx uint64) (types.Account, error) {
	a, err := k.Accounts.Get(ctx, idx)
	if err != nil {
		if errors.Is(err, collections.ErrNotFound) {
			return types.Account{}, types.ErrAccountNotFound.Wrapf("account_index=%d", idx)
		}
		return types.Account{}, err
	}
	a.NormalizeIntFields()
	return a, nil
}

// createAccount is the package-private write primitive used for
// brand-new Account rows. It writes the canonical Accounts entry AND
// every dependent secondary index (OwnerToIndex for masters,
// MasterSubAccounts for sub / pool rows) in one shot, and emits a
// typed `EventAccountUpdated{created=true}` so off-chain indexers can
// rebuild the Accounts table from the event stream alone.
//
// Callers MUST guarantee:
//
//  1. AccountIndex is freshly allocated and not present in `Accounts`
//     (the create path is only fired by EnsureMasterAccount,
//     CreateSubAccount, CreatePublicPoolAccount and InitGenesis).
//  2. OwnerAddress / MasterAccountIndex are the final values for this
//     row's lifetime — both fields are treated as immutable post-create
//     so updateAccount never has to repair the indices.
//
// Splitting create from update lets the update path skip the
// OwnerToIndex Get + compare it used to perform on every AddCollateral
// or UpdateAccountTradingMode call.
func (k Keeper) createAccount(ctx context.Context, a types.Account) error {
	if err := k.Accounts.Set(ctx, a.AccountIndex, a); err != nil {
		return err
	}
	if a.OwnerAddress != "" && a.AccountType == perptypes.MasterAccountType {
		if err := k.OwnerToIndex.Set(ctx, a.OwnerAddress, a.AccountIndex); err != nil {
			return err
		}
	}
	if a.AccountType != perptypes.MasterAccountType && a.MasterAccountIndex != perptypes.NilMasterAccountIndex {
		if err := k.MasterSubAccounts.Set(ctx, collections.Join(a.MasterAccountIndex, a.AccountIndex)); err != nil {
			return err
		}
	}
	return sdk.UnwrapSDKContext(ctx).EventManager().EmitTypedEvent(&types.EventAccountUpdated{
		Account: a,
		Created: true,
	})
}

// updateAccount is the package-private write primitive used for
// in-place mutations of an existing Account row. It writes ONLY the
// canonical Accounts entry and intentionally never touches the
// OwnerToIndex / MasterSubAccounts secondary indices because the
// fields they depend on (OwnerAddress, MasterAccountIndex,
// AccountType) are immutable for the lifetime of an account.
//
// Every successful write emits a typed `EventAccountUpdated{created=false}`
// carrying the full post-write row snapshot, so off-chain consumers can
// keep the canonical Accounts table in sync without polling state.
//
// Cohesive mutators (AddCollateral, UpdateAccountTradingMode,
// UpdatePublicPoolInfo, UpsertPublicPoolShare, ReducePublicPoolShare)
// MUST funnel through this single choke point so the event / metric /
// audit hook lives in exactly one place. External modules MUST use the
// cohesive methods.
func (k Keeper) updateAccount(ctx context.Context, a types.Account) error {
	if err := k.Accounts.Set(ctx, a.AccountIndex, a); err != nil {
		return err
	}
	return sdk.UnwrapSDKContext(ctx).EventManager().EmitTypedEvent(&types.EventAccountUpdated{
		Account: a,
		Created: false,
	})
}

// GetMasterAccountByOwner returns the (master) account associated with the
// given owner address, or ErrAccountNotFound.
func (k Keeper) GetMasterAccountByOwner(ctx context.Context, owner string) (types.Account, error) {
	idx, err := k.OwnerToIndex.Get(ctx, owner)
	if err != nil {
		if errors.Is(err, collections.ErrNotFound) {
			return types.Account{}, types.ErrAccountNotFound.Wrapf("owner=%s", owner)
		}
		return types.Account{}, err
	}
	return k.GetAccount(ctx, idx)
}

// EnsureMasterAccount returns the master account for `owner`, creating one if
// necessary. Used during deposit auto-create.
//
// Only `ErrAccountNotFound` is treated as "missing" so the helper creates a
// new master. Any other error (codec failure, stale OwnerToIndex pointing at
// a deleted account, etc.) is surfaced so callers can't accidentally stamp
// out a second master and orphan the old one.
func (k Keeper) EnsureMasterAccount(ctx context.Context, owner sdk.AccAddress) (types.Account, error) {
	bech := owner.String()
	a, err := k.GetMasterAccountByOwner(ctx, bech)
	if err == nil {
		return a, nil
	}
	if !errors.Is(err, types.ErrAccountNotFound) {
		return types.Account{}, err
	}

	idx, err := k.NextMasterIndex.Next(ctx)
	if err != nil {
		return types.Account{}, err
	}
	if idx < perptypes.FirstUserMasterAccountIndex {
		// Sequence is below the reserved floor (only reachable if
		// genesis was skipped). Skip the wasted Next() loop and jump
		// the counter straight to the floor in a single write.
		idx = perptypes.FirstUserMasterAccountIndex
		if err := k.NextMasterIndex.Set(ctx, idx+1); err != nil {
			return types.Account{}, err
		}
	}
	if idx > perptypes.MaxMasterAccountIndex {
		return types.Account{}, types.ErrAccountIndexExceed.Wrapf("master idx=%d", idx)
	}
	newAcc := types.Account{
		AccountIndex:       idx,
		MasterAccountIndex: perptypes.NilMasterAccountIndex,
		OwnerAddress:       bech,
		AccountType:        perptypes.MasterAccountType,
		AccountTradingMode: perptypes.AccountTradingModeSimple,
		Collateral:         math.ZeroInt(),
		CreatedAt:          sdk.UnwrapSDKContext(ctx).BlockTime().UnixMilli(),
	}
	if err := k.createAccount(ctx, newAcc); err != nil {
		return types.Account{}, err
	}
	return newAcc, nil
}

func (k Keeper) CreateSubAccount(ctx context.Context, master types.Account) (types.Account, error) {
	if master.AccountType != perptypes.MasterAccountType {
		return types.Account{}, types.ErrInvalidAccountType.Wrap("master is not a master account")
	}
	idx, err := k.NextSubIndex.Next(ctx)
	if err != nil {
		return types.Account{}, err
	}
	if idx < perptypes.MinSubAccountIndex {
		// Below the sub-account floor: jump directly with one Set
		// instead of looping Next() and consuming reserved slots.
		idx = perptypes.MinSubAccountIndex
		if err := k.NextSubIndex.Set(ctx, idx+1); err != nil {
			return types.Account{}, err
		}
	}
	if idx > perptypes.MaxAccountIndex {
		return types.Account{}, types.ErrAccountIndexExceed.Wrapf("sub idx=%d", idx)
	}
	sub := types.Account{
		AccountIndex:       idx,
		MasterAccountIndex: master.AccountIndex,
		OwnerAddress:       master.OwnerAddress,
		AccountType:        perptypes.SubAccountType,
		AccountTradingMode: perptypes.AccountTradingModeSimple,
		Collateral:         math.ZeroInt(),
		CreatedAt:          sdk.UnwrapSDKContext(ctx).BlockTime().UnixMilli(),
	}
	if err := k.createAccount(ctx, sub); err != nil {
		return types.Account{}, err
	}
	return sub, nil
}

func (k Keeper) AddCollateral(ctx context.Context, idx uint64, delta math.Int) error {
	a, err := k.GetAccount(ctx, idx)
	if err != nil {
		return err
	}
	a.Collateral = a.Collateral.Add(delta)
	return k.updateAccount(ctx, a)
}

// IsAuthorized returns true if signer can act on account `idx`.
// Sub-accounts inherit the master's OwnerAddress at creation time, so
// the direct string equality covers both master and sub. Owner-less
// accounts (treasury at idx=0, the canonical Insurance Fund pool at
// idx=1) are explicitly rejected so an empty signer can never match
// them — those accounts only mutate through the gov-authority Msg
// paths or genesis.
func (k Keeper) IsAuthorized(ctx context.Context, signer string, idx uint64) (bool, error) {
	a, err := k.GetAccount(ctx, idx)
	if err != nil {
		return false, err
	}
	if a.OwnerAddress == "" {
		return false, nil
	}
	if a.OwnerAddress == signer {
		return true, nil
	}
	return false, nil
}

func (k Keeper) IterateAccounts(ctx context.Context, cb func(types.Account) bool) error {
	iter, err := k.Accounts.Iterate(ctx, nil)
	if err != nil {
		return err
	}
	defer iter.Close()
	for ; iter.Valid(); iter.Next() {
		v, err := iter.Value()
		if err != nil {
			return err
		}
		if cb(v) {
			return nil
		}
	}
	return nil
}

// UpdateAccountTradingMode flips an account's `AccountTradingMode`
// (used by Msg.UpdateAccountConfig). The caller must already have
// validated the new value and ownership/non-pool guards.
func (k Keeper) UpdateAccountTradingMode(ctx context.Context, idx uint64, mode uint32) error {
	a, err := k.GetAccount(ctx, idx)
	if err != nil {
		return err
	}
	a.AccountTradingMode = mode
	return k.updateAccount(ctx, a)
}
