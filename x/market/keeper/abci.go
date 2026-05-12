package keeper

import (
	"context"
	"math"

	"cosmossdk.io/collections"

	sdk "github.com/cosmos/cosmos-sdk/types"

	perptypes "github.com/perpdex/perpdex-l1/types"
)

// EndBlocker walks the ExpiryIndex secondary index and flips every
// market whose ExpiryTimestamp has been crossed by now. The work is
// bounded by `Params.MaxMarketsExpiredPerBlock` so a block where many
// markets expire simultaneously does not stall consensus; any
// remainder is picked up by the next block (entries stay in the index
// until expireMarket removes them).
//
// Failure modes:
//   - liquidationKeeper not wired or ApplyExitPosition returns an
//     error: the market is still flipped to EXPIRED so trading halts,
//     and EventTypeMarketExpireExitFailed is emitted so monitoring can
//     alert. The EndBlocker continues; one failing market does not
//     block the rest (L17).
//   - Index drift (an ExpiryIndex entry whose Market is missing or
//     already EXPIRED): the stale entry is removed and the loop
//     continues.
//   - Params unavailable: returns the underlying error so the chain
//     halts loudly rather than silently skip auto-expiry.
func (k Keeper) EndBlocker(ctx context.Context) error {
	sdkCtx := sdk.UnwrapSDKContext(ctx)
	params, err := k.Params.Get(ctx)
	if err != nil {
		return err
	}
	budget := params.MaxMarketsExpiredPerBlock
	if budget == 0 {
		// Operator emergency switch: governance-only delisting.
		return nil
	}
	now := sdkCtx.BlockTime().UnixMilli()
	rng := new(collections.Range[collections.Pair[int64, uint32]]).
		EndInclusive(collections.Join(now, uint32(math.MaxUint32)))
	iter, err := k.ExpiryIndex.Iterate(ctx, rng)
	if err != nil {
		return err
	}
	defer iter.Close()
	processed := uint32(0)
	for ; iter.Valid() && processed < budget; iter.Next() {
		key, err := iter.Key()
		if err != nil {
			sdkCtx.Logger().Error("market: expiry index iter key", "err", err)
			continue
		}
		idx := key.K2()
		m, err := k.GetMarket(ctx, idx)
		if err != nil {
			// Index drift: market disappeared. Drop the dangling
			// entry so we don't keep paying for it every block.
			sdkCtx.Logger().Error("market: expiry index drift, removing entry",
				"market", idx, "err", err)
			_ = k.ExpiryIndex.Remove(ctx, key)
			continue
		}
		if m.Status != perptypes.MarketStatusActive {
			// Already expired (or in some future non-active state).
			// Drop the index entry to avoid revisiting next block.
			_ = k.ExpiryIndex.Remove(ctx, key)
			continue
		}
		if err := k.expireMarket(ctx, m); err != nil {
			// expireMarket itself rarely errors (it absorbs the
			// liquidation hook error into an event). If it does, we
			// log and continue with the next market so one bad row
			// can't stall block production.
			sdkCtx.Logger().Error("market: expireMarket failed",
				"market", idx, "err", err)
			continue
		}
		processed++
	}
	return nil
}
