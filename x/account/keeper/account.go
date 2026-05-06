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

// GetAccount returns the account with the given index.
func (k Keeper) GetAccount(ctx context.Context, idx uint64) (types.Account, error) {
	a, err := k.Accounts.Get(ctx, idx)
	if err != nil {
		if errors.Is(err, collections.ErrNotFound) {
			return types.Account{}, types.ErrAccountNotFound.Wrapf("account_index=%d", idx)
		}
		return types.Account{}, err
	}
	return a, nil
}

// SetAccount stores an account record.
func (k Keeper) SetAccount(ctx context.Context, a types.Account) error {
	if err := k.Accounts.Set(ctx, a.AccountIndex, a); err != nil {
		return err
	}
	if a.OwnerAddress != "" && a.AccountType == perptypes.MasterAccountType {
		if err := k.OwnerToIndex.Set(ctx, a.OwnerAddress, a.AccountIndex); err != nil {
			return err
		}
	}
	return nil
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
		for idx < perptypes.FirstUserMasterAccountIndex {
			idx, err = k.NextMasterIndex.Next(ctx)
			if err != nil {
				return types.Account{}, err
			}
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
	if err := k.SetAccount(ctx, newAcc); err != nil {
		return types.Account{}, err
	}
	return newAcc, nil
}

// CreateSubAccount mints a new sub account under `master`.
func (k Keeper) CreateSubAccount(ctx context.Context, master types.Account) (types.Account, error) {
	if master.AccountType != perptypes.MasterAccountType {
		return types.Account{}, types.ErrInvalidAccountType.Wrap("master is not a master account")
	}
	idx, err := k.NextSubIndex.Next(ctx)
	if err != nil {
		return types.Account{}, err
	}
	if idx < perptypes.MinSubAccountIndex {
		for idx < perptypes.MinSubAccountIndex {
			idx, err = k.NextSubIndex.Next(ctx)
			if err != nil {
				return types.Account{}, err
			}
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
	if err := k.SetAccount(ctx, sub); err != nil {
		return types.Account{}, err
	}
	return sub, nil
}

// AddCollateral adds amount (math.Int) to the account's collateral.
func (k Keeper) AddCollateral(ctx context.Context, idx uint64, delta math.Int) error {
	a, err := k.GetAccount(ctx, idx)
	if err != nil {
		return err
	}
	if a.Collateral.IsNil() {
		a.Collateral = math.ZeroInt()
	}
	a.Collateral = a.Collateral.Add(delta)
	return k.SetAccount(ctx, a)
}

// SetAccountAsset upserts an account asset row.
func (k Keeper) SetAccountAsset(ctx context.Context, aa types.AccountAsset) error {
	return k.AccountAssets.Set(ctx, collections.Join(aa.AccountIndex, aa.AssetIndex), aa)
}

// GetAccountAsset returns the (account, asset) row, zero-valued if absent.
func (k Keeper) GetAccountAsset(ctx context.Context, accIdx uint64, assetIdx uint32) (types.AccountAsset, error) {
	a, err := k.AccountAssets.Get(ctx, collections.Join(accIdx, assetIdx))
	if err != nil {
		if errors.Is(err, collections.ErrNotFound) {
			return types.AccountAsset{
				AccountIndex:  accIdx,
				AssetIndex:    assetIdx,
				Balance:       math.ZeroInt(),
				LockedBalance: math.ZeroInt(),
				MarginMode:    perptypes.MarginModeDisabled,
			}, nil
		}
		return types.AccountAsset{}, err
	}
	if a.Balance.IsNil() {
		a.Balance = math.ZeroInt()
	}
	if a.LockedBalance.IsNil() {
		a.LockedBalance = math.ZeroInt()
	}
	return a, nil
}

// AddAccountAssetBalance updates the spot balance of an asset.
func (k Keeper) AddAccountAssetBalance(ctx context.Context, accIdx uint64, assetIdx uint32, delta math.Int) error {
	aa, err := k.GetAccountAsset(ctx, accIdx, assetIdx)
	if err != nil {
		return err
	}
	aa.Balance = aa.Balance.Add(delta)
	if aa.Balance.IsNegative() {
		return types.ErrInsufficientFunds.Wrapf("asset_index=%d", assetIdx)
	}
	return k.SetAccountAsset(ctx, aa)
}

// AvailableBalance returns Balance - LockedBalance for an account asset
// (clamped to zero on the very rare nil-state row). Lock-on-place spot
// orders consume Available at place time and release it on cancel /
// evict / fill.
func (k Keeper) AvailableBalance(ctx context.Context, accIdx uint64, assetIdx uint32) (math.Int, error) {
	aa, err := k.GetAccountAsset(ctx, accIdx, assetIdx)
	if err != nil {
		return math.ZeroInt(), err
	}
	bal := aa.Balance
	if bal.IsNil() {
		bal = math.ZeroInt()
	}
	locked := aa.LockedBalance
	if locked.IsNil() {
		locked = math.ZeroInt()
	}
	avail := bal.Sub(locked)
	if avail.IsNegative() {
		return math.ZeroInt(), nil
	}
	return avail, nil
}

// IncreaseLockedBalance reserves `amount` of (account, asset) so the
// matching engine can guarantee a resting spot order has the resources
// to settle. Fails with ErrInsufficientFunds when Available < amount —
// the caller is expected to surface this as an order placement
// rejection rather than continuing.
func (k Keeper) IncreaseLockedBalance(ctx context.Context, accIdx uint64, assetIdx uint32, amount math.Int) error {
	if amount.IsNil() || amount.IsZero() {
		return nil
	}
	if amount.IsNegative() {
		return types.ErrInsufficientFunds.Wrapf("lock amount must be non-negative")
	}
	aa, err := k.GetAccountAsset(ctx, accIdx, assetIdx)
	if err != nil {
		return err
	}
	if aa.Balance.IsNil() {
		aa.Balance = math.ZeroInt()
	}
	if aa.LockedBalance.IsNil() {
		aa.LockedBalance = math.ZeroInt()
	}
	available := aa.Balance.Sub(aa.LockedBalance)
	if available.LT(amount) {
		return types.ErrInsufficientFunds.Wrapf(
			"asset_index=%d available=%s need=%s",
			assetIdx, available.String(), amount.String(),
		)
	}
	aa.LockedBalance = aa.LockedBalance.Add(amount)
	return k.SetAccountAsset(ctx, aa)
}

// DecreaseLockedBalance releases `amount` of previously locked
// (account, asset). Used by orderbook on cancel / evict / fully-filled
// transitions, and by the spot trade application path when a fill
// drains the lock alongside the balance debit. Released amount is
// clamped to the current locked balance so over-release rounds down to
// zero rather than producing a negative lock.
func (k Keeper) DecreaseLockedBalance(ctx context.Context, accIdx uint64, assetIdx uint32, amount math.Int) error {
	if amount.IsNil() || amount.IsZero() {
		return nil
	}
	if amount.IsNegative() {
		return types.ErrInsufficientFunds.Wrapf("release amount must be non-negative")
	}
	aa, err := k.GetAccountAsset(ctx, accIdx, assetIdx)
	if err != nil {
		return err
	}
	if aa.LockedBalance.IsNil() {
		aa.LockedBalance = math.ZeroInt()
	}
	release := amount
	if release.GT(aa.LockedBalance) {
		release = aa.LockedBalance
	}
	aa.LockedBalance = aa.LockedBalance.Sub(release)
	return k.SetAccountAsset(ctx, aa)
}

// IsAuthorized returns true if signer can act on account `idx` (matches owner
// of master, or owner of master for sub-accounts).
func (k Keeper) IsAuthorized(ctx context.Context, signer string, idx uint64) (bool, error) {
	a, err := k.GetAccount(ctx, idx)
	if err != nil {
		return false, err
	}
	if a.OwnerAddress == signer {
		return true, nil
	}
	return false, nil
}

// IterateAccounts walks all accounts in index order.
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

// SetPosition upserts a perp position.
func (k Keeper) SetPosition(ctx context.Context, p types.AccountPosition) error {
	return k.AccountPositions.Set(ctx, collections.Join(p.AccountIndex, p.MarketIndex), p)
}

// GetPosition returns the position; an empty zero-valued one if absent.
func (k Keeper) GetPosition(ctx context.Context, accIdx uint64, marketIdx uint32) (types.AccountPosition, error) {
	p, err := k.AccountPositions.Get(ctx, collections.Join(accIdx, marketIdx))
	if err != nil {
		if errors.Is(err, collections.ErrNotFound) {
			return types.AccountPosition{
				AccountIndex:             accIdx,
				MarketIndex:              marketIdx,
				Position:                 math.ZeroInt(),
				EntryQuote:               math.ZeroInt(),
				LastFundingRatePrefixSum: math.ZeroInt(),
				AllocatedMargin:          math.ZeroInt(),
				MarginMode:               perptypes.CrossMargin,
			}, nil
		}
		return types.AccountPosition{}, err
	}
	if p.Position.IsNil() {
		p.Position = math.ZeroInt()
	}
	if p.EntryQuote.IsNil() {
		p.EntryQuote = math.ZeroInt()
	}
	if p.LastFundingRatePrefixSum.IsNil() {
		p.LastFundingRatePrefixSum = math.ZeroInt()
	}
	if p.AllocatedMargin.IsNil() {
		p.AllocatedMargin = math.ZeroInt()
	}
	return p, nil
}
