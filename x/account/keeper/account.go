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
	a.NormalizeIntFields()
	return a, nil
}

// setAccount is the package-private write primitive for the Account
// row. All cohesive mutator methods (UpdateAccountTradingMode,
// UpdatePublicPoolInfo, CreatePublicPoolAccount, UpsertPublicPoolShare,
// RemovePublicPoolShare, AddCollateral, EnsureMasterAccount,
// CreateSubAccount) funnel through this single choke point so a future
// AccountUpdated event / metric / audit hook can be wired here without
// hunting every caller across the codebase. External modules MUST use
// the cohesive methods; the primitive is intentionally unexported to
// prevent drive-by upserts that bypass invariants and event emission.
//
// The OwnerToIndex write is conditioned on the current pointer being
// missing or stale so idempotent master updates (e.g. AddCollateral on
// an existing master) don't repeatedly rewrite the same value. The
// MasterSubAccounts index is kept in sync here for sub/pool accounts so
// per-master queries can iterate a prefix instead of scanning the
// entire Accounts table.
//
// TODO(events): emit AccountUpdated here once the event schema lands.
func (k Keeper) setAccount(ctx context.Context, a types.Account) error {
	if err := k.Accounts.Set(ctx, a.AccountIndex, a); err != nil {
		return err
	}
	if a.OwnerAddress != "" && a.AccountType == perptypes.MasterAccountType {
		cur, err := k.OwnerToIndex.Get(ctx, a.OwnerAddress)
		switch {
		case err == nil:
			if cur != a.AccountIndex {
				if err := k.OwnerToIndex.Set(ctx, a.OwnerAddress, a.AccountIndex); err != nil {
					return err
				}
			}
		case errors.Is(err, collections.ErrNotFound):
			if err := k.OwnerToIndex.Set(ctx, a.OwnerAddress, a.AccountIndex); err != nil {
				return err
			}
		default:
			return err
		}
	}
	if a.AccountType != perptypes.MasterAccountType && a.MasterAccountIndex != perptypes.NilMasterAccountIndex {
		if err := k.MasterSubAccounts.Set(ctx, collections.Join(a.MasterAccountIndex, a.AccountIndex)); err != nil {
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
	if err := k.setAccount(ctx, newAcc); err != nil {
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
	if err := k.setAccount(ctx, sub); err != nil {
		return types.Account{}, err
	}
	return sub, nil
}

// AddCollateral adds amount (math.Int) to the account's collateral.
//
// TODO(events): emit CollateralChanged here once the event schema lands.
func (k Keeper) AddCollateral(ctx context.Context, idx uint64, delta math.Int) error {
	a, err := k.GetAccount(ctx, idx)
	if err != nil {
		return err
	}
	a.Collateral = a.Collateral.Add(delta)
	return k.setAccount(ctx, a)
}

// setAccountAsset is the package-private write primitive for the
// AccountAsset row. Cohesive mutators (AddAccountAssetBalance,
// IncreaseLockedBalance, DecreaseLockedBalance,
// SetAccountAssetMarginMode, TransferAccountAssetBalance) funnel
// through here so spot balance / lock changes have a single choke
// point for future event emission.
//
// TODO(events): emit SpotBalanceChanged here once the event schema lands.
func (k Keeper) setAccountAsset(ctx context.Context, aa types.AccountAsset) error {
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
	a.NormalizeIntFields()
	return a, nil
}

// AddAccountAssetBalance updates the spot balance of an asset.
//
// Positive deltas (credits) only validate the post-state balance is
// non-negative. Negative deltas (debits) MUST respect the resting
// `LockedBalance` reservation: only the available portion
// (`Balance - LockedBalance`) may be withdrawn so user-facing
// Withdraw / Transfer cannot raid spot orders' locked collateral.
// The matching engine continues to use TransferAccountAssetBalance for
// fill-driven movements, which knows when to drain the lock first.
func (k Keeper) AddAccountAssetBalance(ctx context.Context, accIdx uint64, assetIdx uint32, delta math.Int) error {
	aa, err := k.GetAccountAsset(ctx, accIdx, assetIdx)
	if err != nil {
		return err
	}
	if delta.IsNegative() {
		debit := delta.Neg()
		available := aa.Balance.Sub(aa.LockedBalance)
		if available.LT(debit) {
			return types.ErrInsufficientFunds.Wrapf(
				"asset_index=%d available=%s need=%s",
				assetIdx, available.String(), debit.String(),
			)
		}
	}
	aa.Balance = aa.Balance.Add(delta)
	if aa.Balance.IsNegative() {
		return types.ErrInsufficientFunds.Wrapf("asset_index=%d", assetIdx)
	}
	return k.setAccountAsset(ctx, aa)
}

// SetAccountAssetMarginMode toggles the spot row's MarginMode flag
// (used by Msg.UpdateAccountAssetConfig). Auto-creates the row when
// missing so a fresh master account can opt into margin without first
// holding a balance.
func (k Keeper) SetAccountAssetMarginMode(ctx context.Context, accIdx uint64, assetIdx uint32, mode uint32) error {
	aa, err := k.GetAccountAsset(ctx, accIdx, assetIdx)
	if err != nil {
		return err
	}
	aa.AccountIndex = accIdx
	aa.AssetIndex = assetIdx
	aa.MarginMode = mode
	return k.setAccountAsset(ctx, aa)
}

// TransferAccountAssetBalance moves `amount` of `assetIdx` from
// `from` to `to` in a single atomic step. When `drainLockedFirst` is
// true the source's locked portion is drained ahead of the available
// portion (lock-on-place semantics for resting spot makers); when
// false the source must have enough Available (Balance - Locked) to
// cover the amount (taker / fee path).
//
// On insufficient funds the function returns
// types.ErrInsufficientFunds; callers in x/trade re-wrap into the
// maker / taker sentinels so the matching loop can recover. The
// debit + credit always run in a single keeper call so a partial
// failure cannot leave one side updated.
func (k Keeper) TransferAccountAssetBalance(
	ctx context.Context,
	from, to uint64,
	assetIdx uint32,
	amount math.Int,
	drainLockedFirst bool,
) error {
	if amount.IsNil() || amount.IsZero() {
		return nil
	}
	if amount.IsNegative() {
		return types.ErrInsufficientFunds.Wrap("transfer amount must be non-negative")
	}
	src, err := k.GetAccountAsset(ctx, from, assetIdx)
	if err != nil {
		return err
	}
	if drainLockedFirst {
		// Maker path: balance must cover full amount; the lock is
		// drained first so a partial fill releases the proportional
		// portion of resources reserved at place time.
		if src.Balance.LT(amount) {
			return types.ErrInsufficientFunds.Wrapf(
				"account %d asset %d have %s need %s",
				from, assetIdx, src.Balance.String(), amount.String())
		}
	} else {
		// Taker path: only the available portion (Balance - Locked)
		// can be debited so a resting lock cannot be raided.
		available := src.Balance.Sub(src.LockedBalance)
		if available.LT(amount) {
			return types.ErrInsufficientFunds.Wrapf(
				"account %d asset %d available %s need %s",
				from, assetIdx, available.String(), amount.String())
		}
	}
	dst, err := k.GetAccountAsset(ctx, to, assetIdx)
	if err != nil {
		return err
	}
	if drainLockedFirst {
		drain := amount
		if drain.GT(src.LockedBalance) {
			drain = src.LockedBalance
		}
		src.LockedBalance = src.LockedBalance.Sub(drain)
	}
	src.Balance = src.Balance.Sub(amount)
	dst.Balance = dst.Balance.Add(amount)
	if err := k.setAccountAsset(ctx, src); err != nil {
		return err
	}
	return k.setAccountAsset(ctx, dst)
}

// AvailableBalance returns Balance - LockedBalance for an account asset.
// Lock-on-place spot orders consume Available at place time and release
// it on cancel / evict / fill.
//
// A negative result indicates a state inconsistency (Balance dropped
// below LockedBalance). The function still clamps to zero to keep
// downstream math safe but emits a high-severity log line so the
// invariant breach is observable in node logs.
func (k Keeper) AvailableBalance(ctx context.Context, accIdx uint64, assetIdx uint32) (math.Int, error) {
	aa, err := k.GetAccountAsset(ctx, accIdx, assetIdx)
	if err != nil {
		return math.ZeroInt(), err
	}
	avail := aa.Balance.Sub(aa.LockedBalance)
	if avail.IsNegative() {
		sdk.UnwrapSDKContext(ctx).Logger().
			With("module", "x/"+types.ModuleName).
			Error(
				"available balance underflow",
				"account_index", accIdx,
				"asset_index", assetIdx,
				"balance", aa.Balance.String(),
				"locked_balance", aa.LockedBalance.String(),
			)
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
	available := aa.Balance.Sub(aa.LockedBalance)
	if available.LT(amount) {
		return types.ErrInsufficientFunds.Wrapf(
			"asset_index=%d available=%s need=%s",
			assetIdx, available.String(), amount.String(),
		)
	}
	aa.LockedBalance = aa.LockedBalance.Add(amount)
	return k.setAccountAsset(ctx, aa)
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
	release := amount
	if release.GT(aa.LockedBalance) {
		release = aa.LockedBalance
	}
	aa.LockedBalance = aa.LockedBalance.Sub(release)
	return k.setAccountAsset(ctx, aa)
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

// setPosition is the package-private write primitive for the
// AccountPosition row. Cohesive mutators (UpdatePosition,
// SetPositionLeverage) funnel through here so position state has a
// single choke point for future event emission.
//
// TODO(events): emit PositionUpdated here once the event schema lands.
func (k Keeper) setPosition(ctx context.Context, p types.AccountPosition) error {
	return k.AccountPositions.Set(ctx, collections.Join(p.AccountIndex, p.MarketIndex), p)
}

// UpdatePosition is the canonical read-modify-write wrapper for
// `AccountPosition`. It loads the position (auto-vivifying a zero-
// valued record when missing — same semantics as `GetPosition`),
// runs the supplied `mut` callback against a mutable pointer, then
// persists the result through the package-private `setPosition`.
//
// Cross-module callers (x/trade, x/funding, x/liquidation) own the
// mutation logic in their own keeper but no longer touch the
// underlying setter directly; this is what lets x/account add
// invariants / events / metrics in exactly one place once the
// schema lands.
//
// If `mut` returns an error the position is NOT persisted (so a
// caller can short-circuit on bounds violations like
// `errPositionOutOfBounds`). The returned `AccountPosition` is the
// post-mutation value.
func (k Keeper) UpdatePosition(
	ctx context.Context,
	accIdx uint64,
	marketIdx uint32,
	mut func(*types.AccountPosition) error,
) (types.AccountPosition, error) {
	pos, err := k.GetPosition(ctx, accIdx, marketIdx)
	if err != nil {
		return types.AccountPosition{}, err
	}
	pos.AccountIndex = accIdx
	pos.MarketIndex = marketIdx
	if err := mut(&pos); err != nil {
		return types.AccountPosition{}, err
	}
	if err := k.setPosition(ctx, pos); err != nil {
		return types.AccountPosition{}, err
	}
	return pos, nil
}

// SetPositionLeverage flips a position's `MarginMode` and
// `InitialMarginFraction`. Used by Msg.UpdateLeverage; the caller
// has already validated the position is empty and the imf falls
// inside [market_min, MarginTick].
func (k Keeper) SetPositionLeverage(
	ctx context.Context,
	accIdx uint64,
	marketIdx uint32,
	marginMode uint32,
	imf uint32,
) error {
	_, err := k.UpdatePosition(ctx, accIdx, marketIdx, func(p *types.AccountPosition) error {
		p.MarginMode = marginMode
		p.InitialMarginFraction = imf
		return nil
	})
	return err
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
	return k.setAccount(ctx, a)
}

// UpdatePublicPoolInfo loads the pool account, runs `mut` against
// its `PublicPoolInfo` pointer, and persists. Returns the updated
// account. If the loaded account is not a pool / has no
// PublicPoolInfo the caller gets `ErrInvalidPoolAccount`. If `mut`
// returns an error the account is NOT persisted.
//
// Replaces the GetAccount -> mutate info -> SetAccount pattern that
// used to be inlined in UpdatePublicPool / MintShares / BurnShares /
// StrategyTransfer.
func (k Keeper) UpdatePublicPoolInfo(
	ctx context.Context,
	idx uint64,
	mut func(*types.PublicPoolInfo) error,
) (types.Account, error) {
	a, err := k.GetAccount(ctx, idx)
	if err != nil {
		return types.Account{}, err
	}
	if a.PublicPoolInfo == nil {
		return types.Account{}, types.ErrInvalidPoolAccount.Wrapf("account %d", idx)
	}
	if err := mut(a.PublicPoolInfo); err != nil {
		return types.Account{}, err
	}
	if err := k.setAccount(ctx, a); err != nil {
		return types.Account{}, err
	}
	return a, nil
}

// PublicPoolAccountParams carries the inputs needed to mint a fresh
// PUBLIC_POOL / INSURANCE_FUND sub-account. Callers fill the master
// reference and the seed PublicPoolInfo; the keeper handles index
// allocation and timestamping.
type PublicPoolAccountParams struct {
	Master             types.Account
	AccountType        uint32
	AccountTradingMode uint32
	SeedCollateral     math.Int
	Info               *types.PublicPoolInfo
}

// CreatePublicPoolAccount allocates the next sub-account index and
// persists a brand-new pool account in a single step. Used by
// Msg.CreatePublicPool.
func (k Keeper) CreatePublicPoolAccount(
	ctx context.Context,
	p PublicPoolAccountParams,
) (types.Account, error) {
	idx, err := k.allocatePoolSubAccountIndex(ctx)
	if err != nil {
		return types.Account{}, err
	}
	now := sdk.UnwrapSDKContext(ctx).BlockTime().UnixMilli()
	pool := types.Account{
		AccountIndex:       idx,
		MasterAccountIndex: p.Master.AccountIndex,
		OwnerAddress:       p.Master.OwnerAddress,
		AccountType:        p.AccountType,
		AccountTradingMode: p.AccountTradingMode,
		Collateral:         p.SeedCollateral,
		CreatedAt:          now,
		PublicPoolInfo:     p.Info,
	}
	if err := k.setAccount(ctx, pool); err != nil {
		return types.Account{}, err
	}
	return pool, nil
}

// allocatePoolSubAccountIndex pulls the next sub-account index,
// skipping any reserved range. Mirrors CreateSubAccount's allocation
// without the master-type guard so the IF master (nil owner) can
// also spawn sub-accounts. Internal helper used by
// CreatePublicPoolAccount.
func (k Keeper) allocatePoolSubAccountIndex(ctx context.Context) (uint64, error) {
	idx, err := k.NextSubIndex.Next(ctx)
	if err != nil {
		return 0, err
	}
	if idx < perptypes.MinSubAccountIndex {
		idx = perptypes.MinSubAccountIndex
		if err := k.NextSubIndex.Set(ctx, idx+1); err != nil {
			return 0, err
		}
	}
	if idx > perptypes.MaxAccountIndex {
		return 0, types.ErrAccountIndexExceed.Wrapf("sub idx=%d", idx)
	}
	return idx, nil
}

// UpsertPublicPoolShare appends or replaces the PublicPoolShare
// entry for `poolIdx` on the master account. Used by MintShares to
// fold a fresh deposit into the LP row.
//
// Returns ErrSharesListFull when no entry exists and the per-master
// share-list cap is reached.
func (k Keeper) UpsertPublicPoolShare(
	ctx context.Context,
	masterIdx, poolIdx uint64,
	shareDelta, principalDelta math.Int,
	entryTimestamp int64,
) error {
	master, err := k.GetAccount(ctx, masterIdx)
	if err != nil {
		return err
	}
	if i, ok := FindShareEntry(master, poolIdx); ok {
		master.PublicPoolShares[i].ShareAmount = master.PublicPoolShares[i].ShareAmount.Add(shareDelta)
		master.PublicPoolShares[i].PrincipalAmount = master.PublicPoolShares[i].PrincipalAmount.Add(principalDelta)
		master.PublicPoolShares[i].EntryTimestamp = entryTimestamp
	} else {
		if uint32(len(master.PublicPoolShares)) >= uint32(perptypes.SharesListSize) {
			return types.ErrSharesListFull
		}
		master.PublicPoolShares = append(master.PublicPoolShares, types.PublicPoolShare{
			PublicPoolIndex: poolIdx,
			ShareAmount:     shareDelta,
			PrincipalAmount: principalDelta,
			EntryTimestamp:  entryTimestamp,
		})
	}
	return k.setAccount(ctx, master)
}

// ReducePublicPoolShare debits `shareAmount` (and the proportional
// principal) from the master's existing PublicPoolShare for
// `poolIdx`. When the resulting `ShareAmount` reaches zero the entry
// is removed entirely. Used by BurnShares.
//
// Caller must have already validated the entry exists with at least
// `shareAmount` shares (ErrInsufficientShares is the standard
// surface for a missing/under-funded entry).
func (k Keeper) ReducePublicPoolShare(
	ctx context.Context,
	masterIdx, poolIdx uint64,
	shareAmount math.Int,
) error {
	master, err := k.GetAccount(ctx, masterIdx)
	if err != nil {
		return err
	}
	entryIdx, ok := FindShareEntry(master, poolIdx)
	if !ok {
		return types.ErrInsufficientShares.Wrap("depositor has no entry for this pool")
	}
	entry := master.PublicPoolShares[entryIdx]
	if entry.ShareAmount.LT(shareAmount) {
		return types.ErrInsufficientShares.Wrapf(
			"requested %s, have %s",
			shareAmount.String(), entry.ShareAmount.String(),
		)
	}
	principalDelta := entry.PrincipalAmount.Mul(shareAmount).Quo(entry.ShareAmount)
	entry.ShareAmount = entry.ShareAmount.Sub(shareAmount)
	entry.PrincipalAmount = entry.PrincipalAmount.Sub(principalDelta)
	if entry.ShareAmount.IsZero() {
		master.PublicPoolShares = append(master.PublicPoolShares[:entryIdx], master.PublicPoolShares[entryIdx+1:]...)
	} else {
		master.PublicPoolShares[entryIdx] = entry
	}
	return k.setAccount(ctx, master)
}

// GetPosition returns the position; an empty zero-valued one if absent.
func (k Keeper) GetPosition(ctx context.Context, accIdx uint64, marketIdx uint32) (types.AccountPosition, error) {
	p, err := k.AccountPositions.Get(ctx, collections.Join(accIdx, marketIdx))
	if err != nil {
		if errors.Is(err, collections.ErrNotFound) {
			return types.AccountPosition{
				AccountIndex:             accIdx,
				MarketIndex:              marketIdx,
				BaseSize:                 math.ZeroInt(),
				EntryQuote:               math.ZeroInt(),
				LastFundingRatePrefixSum: math.ZeroInt(),
				AllocatedMargin:          math.ZeroInt(),
				MarginMode:               perptypes.CrossMargin,
			}, nil
		}
		return types.AccountPosition{}, err
	}
	p.NormalizeIntFields()
	return p, nil
}

// IterateAccountPositions walks every persisted AccountPosition row owned
// by `accountIdx`. The callback returns `true` to stop early.
//
// Replaces the old MaxPerpsMarketIndex-wide loops in
// risk.ComputeCrossRisk / IsValidRiskChangeFrom / SnapshotRisk /
// IterateIsolatedPositions / liquidation.processAccount /
// rankVictimPositionsByUPnL / account.settleAllPositionFunding which each
// did up to 256 GetPosition reads per call. With this iterator we only
// touch persisted rows.
//
// Callers may still see Position == 0 rows (the keeper does not delete
// positions when they net to zero, only when funding is settled and the
// position is closed); skip them with `pos.BaseSize.IsZero()` if the
// caller cares about non-empty positions only.
func (k Keeper) IterateAccountPositions(
	ctx context.Context,
	accountIdx uint64,
	cb func(types.AccountPosition) bool,
) error {
	rng := collections.NewPrefixedPairRange[uint64, uint32](accountIdx)
	iter, err := k.AccountPositions.Iterate(ctx, rng)
	if err != nil {
		return err
	}
	defer iter.Close()
	for ; iter.Valid(); iter.Next() {
		p, err := iter.Value()
		if err != nil {
			return err
		}
		p.NormalizeIntFields()
		if cb(p) {
			return nil
		}
	}
	return nil
}
