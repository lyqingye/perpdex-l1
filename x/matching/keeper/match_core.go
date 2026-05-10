package keeper

import (
	"context"
	"errors"
	"strconv"

	sdk "github.com/cosmos/cosmos-sdk/types"

	perptypes "github.com/perpdex/perpdex-l1/types"
	"github.com/perpdex/perpdex-l1/x/matching/types"
	orderbooktypes "github.com/perpdex/perpdex-l1/x/orderbook/types"
	tradekeeper "github.com/perpdex/perpdex-l1/x/trade/keeper"
	tradetypes "github.com/perpdex/perpdex-l1/x/trade/types"
)

// errTakerRejected is the sentinel returned by `applyPerpFill` /
// `applySpotFill` when the trade engine rejects a fill with a
// recoverable taker-side error (Lighter parity with
// `cancel_taker_order` in matching_engine.rs). The outer loop must
// preserve already-committed fills, terminate, and (for the user
// path) force-cancel the residue rather than rest it on the book.
var errTakerRejected = errors.New("matching: taker recoverable rejection")

// nextMaker peeks the best opposite resting order for `taker` and
// transparently evicts on the OUTER ctx any maker that cannot trade
// (expired GTT, reduce-only direction invalid for the taker side),
// re-peeking until a usable maker is found or the loop terminates.
//
// Returns (maker, true, nil) when a tradable maker is at the front
// of the opposite book. Returns (zero, false, nil) when:
//
//   - the book is empty;
//   - the limit-style taker price is unreachable (best is worse);
//   - self-trade prevention triggers;
//   - the taker's reduce-only invariant is violated against its own
//     current position.
//
// In every false case the outer loop should stop matching: nothing
// the caller can do will change the answer this iteration.
//
// The internal eviction-and-retry loop absorbs the "skip this maker
// and try the next" cases so callers do not have to distinguish
// "skip" from "stop" — they collapse into the single boolean return.
// CONTINUE-style outcomes already mutated the book on the outer ctx
// (eviction is a public state change), and the next call to nextMaker
// will start from the new best.
func (k Keeper) nextMaker(
	ctx context.Context,
	taker *orderbooktypes.Order,
	isPerp bool,
	now int64,
) (orderbooktypes.OrderBookEntry, bool, error) {
	for {
		best, ok, err := k.bookKeeper.PeekBestOpposite(ctx, taker.MarketIndex, taker.IsAsk)
		if err != nil {
			return orderbooktypes.OrderBookEntry{}, false, err
		}
		if !ok {
			return orderbooktypes.OrderBookEntry{}, false, nil
		}
		// MarketOrder has no limit price (Price==0); the price-
		// reachable check only applies to limit-style orders. This
		// prevents a buy MarketOrder (or a triggered STOP/TAKE that
		// became a buy MarketOrder) from being self-cancelled by
		// 0 < best.Price.
		if taker.OrderType != perptypes.MarketOrder {
			if taker.IsAsk && taker.Price > best.Price {
				return orderbooktypes.OrderBookEntry{}, false, nil
			}
			if !taker.IsAsk && taker.Price < best.Price {
				return orderbooktypes.OrderBookEntry{}, false, nil
			}
		}
		if best.OwnerAccountIndex == taker.OwnerAccountIndex {
			// Self-trade prevention: cancel taker remainder.
			return orderbooktypes.OrderBookEntry{}, false, nil
		}
		if best.Expiry > 0 && now >= best.Expiry {
			// EvictMakerOrder removes the entry, marks the maker
			// Order as Cancelled, and clears its client / account-
			// open indexes so the now-gone resting order does not
			// linger as a stale "open" record.
			if _, err := k.bookKeeper.EvictMakerOrder(ctx, best.OrderIndex, perptypes.OrderStatusCancelled); err != nil {
				return orderbooktypes.OrderBookEntry{}, false, err
			}
			continue
		}
		// Reduce-only invariant for the taker: if the taker is
		// reduce-only, the fill must only reduce (never grow) the
		// taker's current position absolute value.
		if isPerp && taker.ReduceOnly {
			pos, err := k.accountKeeper.GetPosition(ctx, taker.OwnerAccountIndex, taker.MarketIndex)
			if err != nil {
				return orderbooktypes.OrderBookEntry{}, false, err
			}
			if pos.BaseSize.IsZero() ||
				(taker.IsAsk && !pos.OpeningIsBid()) ||
				(!taker.IsAsk && !pos.OpeningIsAsk()) {
				return orderbooktypes.OrderBookEntry{}, false, nil
			}
		}
		// Maker reduce-only direction check: a reduce-only maker
		// that no longer holds an opposite position is invalid and
		// must be evicted; previously the entry was dropped but
		// the Order record + indexes leaked, leaving a phantom
		// "open" order.
		if isPerp && best.ReduceOnly {
			pos, err := k.accountKeeper.GetPosition(ctx, best.OwnerAccountIndex, taker.MarketIndex)
			if err != nil {
				return orderbooktypes.OrderBookEntry{}, false, err
			}
			if pos.BaseSize.IsZero() ||
				(taker.IsAsk && pos.OpeningIsBid()) ||
				(!taker.IsAsk && pos.OpeningIsAsk()) {
				if _, err := k.bookKeeper.EvictMakerOrder(ctx, best.OrderIndex, perptypes.OrderStatusCancelled); err != nil {
					return orderbooktypes.OrderBookEntry{}, false, err
				}
				continue
			}
		}
		return best, true, nil
	}
}

// matchSize computes the trade base for a `(taker, maker)` pair,
// applying both sides' reduce-only caps so a single fill cannot flip
// either account to the opposite side.
//
// Returns (base, true, nil) when a non-zero size is admissible.
// Returns (0, false, nil) when sizing collapses to zero (e.g. the
// taker's reduce-only |position| is already 0 or, defensively, the
// maker's is — the latter should already have been evicted by
// nextMaker but the guard keeps matchSize self-contained).
//
// matchSize never mutates orderbook state; nextMaker owns the
// outer-ctx eviction policy. When ok=false the outer loop should
// stop matching for this taker.
func (k Keeper) matchSize(
	ctx context.Context,
	taker *orderbooktypes.Order,
	maker orderbooktypes.OrderBookEntry,
	isPerp bool,
) (uint64, bool, error) {
	base := taker.RemainingBaseAmount
	if base > maker.RemainingBaseAmount {
		base = maker.RemainingBaseAmount
	}
	// Cap reduce-only fills to the taker's current position size so a
	// single trade cannot flip the account to the opposite side.
	if isPerp && taker.ReduceOnly {
		pos, err := k.accountKeeper.GetPosition(ctx, taker.OwnerAccountIndex, taker.MarketIndex)
		if err != nil {
			return 0, false, err
		}
		limit := pos.BaseSize.Abs().Uint64()
		if limit == 0 {
			return 0, false, nil
		}
		if base > limit {
			base = limit
		}
	}
	// Symmetric cap on the maker side. nextMaker has already evicted
	// reduce-only makers whose position is zero / wrong-sided, so
	// makerLimit > 0 is the expected steady state. The zero-limit
	// branch is kept as defense in depth: if it ever fires, treat it
	// as "no trade possible" without attempting another eviction —
	// nextMaker will re-evaluate on the next outer iteration.
	if isPerp && maker.ReduceOnly && base > 0 {
		pos, err := k.accountKeeper.GetPosition(ctx, maker.OwnerAccountIndex, taker.MarketIndex)
		if err != nil {
			return 0, false, err
		}
		makerLimit := pos.BaseSize.Abs().Uint64()
		if makerLimit == 0 {
			return 0, false, nil
		}
		if base > makerLimit {
			base = makerLimit
		}
	}
	if base == 0 {
		return 0, false, nil
	}
	return base, true, nil
}

// applyPerpFill commits one perp matching iteration to a cache ctx,
// updates the maker orderbook entry inside the same cache, and
// writes the cache atomically on success.
//
// The caller (applyUserFill / applyLiquidationFill) constructs the
// `tradekeeper.PerpFill` so each path can supply its own fee /
// liquidation-routing fields without leaking those concerns into
// the matching-loop kernel.
//
// Returns:
//
//   - (true,  nil)               : fill applied, writeCache invoked.
//   - (false, nil)               : maker recoverable error; the bad
//     maker has been evicted on the OUTER ctx so the next outer
//     iteration peeks past it. Outer loop should continue.
//   - (false, errTakerRejected)  : taker recoverable error; outer
//     loop should preserve prior fills, stop, and (for the user
//     path) force-cancel residue.
//   - (false, other err)         : hard failure; propagate to revert
//     the entire Msg.
func (k Keeper) applyPerpFill(
	ctx context.Context,
	maker orderbooktypes.OrderBookEntry,
	fill tradekeeper.PerpFill,
) (bool, error) {
	sdkCtx := sdk.UnwrapSDKContext(ctx)
	cacheCtx, writeCache := sdkCtx.CacheContext()
	err := k.tradeKeeper.ApplyPerpsMatching(cacheCtx, fill)
	if err == nil {
		if _, fmErr := k.bookKeeper.FillMakerOrder(cacheCtx, maker.OrderIndex, fill.BaseAmount); fmErr != nil {
			err = fmErr
		}
	}
	if err == nil {
		writeCache()
		return true, nil
	}
	return k.classifyApplyError(ctx, fill.MarketIndex, maker.OrderIndex, err)
}

// applySpotFill is the spot-market counterpart of applyPerpFill. The
// transaction semantics are identical (cache + apply + maker book
// update + writeCache); only the trade-engine entry point differs.
func (k Keeper) applySpotFill(
	ctx context.Context,
	maker orderbooktypes.OrderBookEntry,
	fill tradekeeper.SpotFill,
	baseAssetID, quoteAssetID uint32,
) (bool, error) {
	sdkCtx := sdk.UnwrapSDKContext(ctx)
	cacheCtx, writeCache := sdkCtx.CacheContext()
	err := k.tradeKeeper.ApplySpotMatching(cacheCtx, fill, baseAssetID, quoteAssetID)
	if err == nil {
		if _, fmErr := k.bookKeeper.FillMakerOrder(cacheCtx, maker.OrderIndex, fill.BaseAmount); fmErr != nil {
			err = fmErr
		}
	}
	if err == nil {
		writeCache()
		return true, nil
	}
	return k.classifyApplyError(ctx, fill.MarketIndex, maker.OrderIndex, err)
}

// classifyApplyError translates a trade-engine error from a single
// (apply + FillMakerOrder) cache attempt into the matching loop's
// recoverable-error vocabulary, performing the outer-ctx side-effect
// each branch requires (maker eviction event + book mutation; or a
// taker-aborted event). It is the only place the matching keeper
// inspects tradetypes recoverable sentinels, keeping the apply
// helpers free of policy.
func (k Keeper) classifyApplyError(
	ctx context.Context,
	marketIdx uint32,
	makerOrderIdx uint64,
	applyErr error,
) (bool, error) {
	sdkCtx := sdk.UnwrapSDKContext(ctx)
	switch {
	case tradetypes.IsRecoverableMakerError(applyErr):
		// Discard cache, evict the bad maker on the OUTER ctx so
		// the next loop iteration won't peek the same resting
		// order.
		if _, err := k.bookKeeper.EvictMakerOrder(ctx, makerOrderIdx, perptypes.OrderStatusCancelled); err != nil {
			return false, err
		}
		sdkCtx.EventManager().EmitEvent(sdk.NewEvent(
			types.EventTypeMakerEvictedBadState,
			sdk.NewAttribute(types.AttributeKeyMarketIndex, strconv.FormatUint(uint64(marketIdx), 10)),
			sdk.NewAttribute(types.AttributeKeyOrderIndex, strconv.FormatUint(makerOrderIdx, 10)),
			sdk.NewAttribute(types.AttributeKeyReason, applyErr.Error()),
		))
		return false, nil
	case tradetypes.IsRecoverableTakerError(applyErr):
		// Discard cache. Previously committed fills (any writeCache
		// calls in earlier iterations) are retained; the residue
		// is force-cancelled by the outer loop — the taker just
		// proved it cannot satisfy further fills, so resting it on
		// the book would only re-trigger the same failure for
		// downstream takers. Lighter parity: `cancel_taker_order`
		// pops the taker register regardless of TIF.
		sdkCtx.EventManager().EmitEvent(sdk.NewEvent(
			types.EventTypeTakerAbortedBadState,
			sdk.NewAttribute(types.AttributeKeyMarketIndex, strconv.FormatUint(uint64(marketIdx), 10)),
			sdk.NewAttribute(types.AttributeKeyReason, applyErr.Error()),
		))
		return false, errTakerRejected
	default:
		// Hard error: discard cache and propagate so the whole
		// Msg reverts atomically.
		return false, applyErr
	}
}

// emitOrderFill emits the per-iteration `order_fill` telemetry event
// shared by both the user and liquidation matching loops.
func (k Keeper) emitOrderFill(ctx context.Context, marketIdx uint32, price uint32, base uint64) {
	sdk.UnwrapSDKContext(ctx).EventManager().EmitEvent(sdk.NewEvent(
		types.EventTypeOrderFill,
		sdk.NewAttribute(types.AttributeKeyMarketIndex, strconv.FormatUint(uint64(marketIdx), 10)),
		sdk.NewAttribute(types.AttributeKeyPrice, strconv.FormatUint(uint64(price), 10)),
		sdk.NewAttribute(types.AttributeKeyBase, strconv.FormatUint(base, 10)),
	))
}
