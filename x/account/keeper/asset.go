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

// setAccountAsset is the package-private write primitive for the
// AccountAsset row. Cohesive mutators (AddAccountAssetBalance,
// IncreaseLockedBalance, DecreaseLockedBalance,
// SetAccountAssetMarginMode, TransferAccountAssetBalance) funnel
// through here so spot balance / lock changes have a single choke
// point for event emission. Every successful write fires an
// `EventAccountAssetUpdated` typed event carrying the full
// post-write row snapshot, so off-chain consumers (indexers, risk
// engines) can rebuild AccountAssets from the event stream alone.
func (k Keeper) setAccountAsset(ctx context.Context, aa types.AccountAsset) error {
	if err := k.AccountAssets.Set(ctx, collections.Join(aa.AccountIndex, aa.AssetIndex), aa); err != nil {
		return err
	}
	return sdk.UnwrapSDKContext(ctx).EventManager().EmitTypedEvent(&types.EventAccountAssetUpdated{
		AccountAsset: aa,
	})
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
