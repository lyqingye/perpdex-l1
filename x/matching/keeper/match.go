package keeper

import (
	"context"
	"strconv"

	sdk "github.com/cosmos/cosmos-sdk/types"

	perptypes "github.com/perpdex/perpdex-l1/types"
	"github.com/perpdex/perpdex-l1/x/matching/types"
	orderbooktypes "github.com/perpdex/perpdex-l1/x/orderbook/types"
	tradekeeper "github.com/perpdex/perpdex-l1/x/trade/keeper"
	tradetypes "github.com/perpdex/perpdex-l1/x/trade/types"
)

// matchOrder runs the 12-step matching loop for a taker order. It returns the
// resulting filled base amount and a status (open/partially_filled/filled/cancelled).
//
// The 12 steps from 17-matching.md §3:
//  1. trigger check (handled before this fn for stop/take orders)
//  2. reduce-only invariant (taker side reducing existing position)
//  3. empty-book (POST_ONLY ok; IOC fail; GTT rest)
//  4. POST_ONLY cross check
//  5. price unreachable (limit_price worse than best opposite)
//  6. self-trade prevention
//  7. maker GTT expiry skip
//  8. maker reduce-only invariant
//  9. trade_base = min(remaining_taker, remaining_maker)
//
// 10. fee accounting (delegated to ApplyPerpsMatching/ApplySpotMatching)
// 11. apply matching via trade keeper
// 12. orderbook update (PartialFill / Remove)
func (k Keeper) matchOrder(ctx context.Context, taker *orderbooktypes.Order, maxFills uint32) (uint64, uint32, error) {
	now := sdk.UnwrapSDKContext(ctx).BlockTime().UnixMilli()
	market, err := k.marketKeeper.GetMarket(ctx, taker.MarketIndex)
	if err != nil {
		return 0, perptypes.OrderStatusCancelled, err
	}
	isPerp := market.MarketType == perptypes.MarketTypePerps

	var totalFilled uint64
	var fills uint32
	for taker.RemainingBaseAmount > 0 {
		if fills >= maxFills {
			return totalFilled, perptypes.OrderStatusPartiallyFilled, nil
		}

		best, ok, err := k.bookKeeper.PeekBestOpposite(ctx, taker.MarketIndex, taker.IsAsk)
		if err != nil {
			return totalFilled, perptypes.OrderStatusCancelled, err
		}
		if !ok {
			break
		}
		// MarketOrder has no limit price (Price==0); the price-reachable
		// check only applies to limit-style orders. This prevents a buy
		// MarketOrder (or a triggered STOP/TAKE that became a buy
		// MarketOrder) from being self-cancelled by 0 < best.Price.
		if taker.OrderType != perptypes.MarketOrder {
			if taker.IsAsk && taker.Price > best.Price {
				break
			}
			if !taker.IsAsk && taker.Price < best.Price {
				break
			}
		}
		if best.OwnerAccountIndex == taker.OwnerAccountIndex {
			// Self-trade prevention: cancel taker remainder.
			break
		}
		if best.Expiry > 0 && now >= best.Expiry {
			// EvictMakerOrder removes the entry, marks the maker
			// Order as Cancelled, and clears its client / account-
			// open indexes so the now-gone resting order does not
			// linger as a stale "open" record. Previously only the
			// entry was removed and the orderbook GTT EndBlocker had
			// to retroactively clean up.
			if _, err := k.bookKeeper.EvictMakerOrder(ctx, best.OrderIndex, perptypes.OrderStatusCancelled); err != nil {
				return totalFilled, perptypes.OrderStatusCancelled, err
			}
			continue
		}
		// Reduce-only invariant for the taker: if the taker is reduce-only,
		// the fill must only reduce (never grow) the taker's current
		// position absolute value. Taker side (isAsk=true ⇒ sell, reducing
		// a long position).
		if isPerp && taker.ReduceOnly {
			pos, err := k.accountKeeper.GetPosition(ctx, taker.OwnerAccountIndex, taker.MarketIndex)
			if err != nil {
				return totalFilled, perptypes.OrderStatusCancelled, err
			}
			if pos.Position.IsZero() ||
				(taker.IsAsk && !pos.Position.IsPositive()) ||
				(!taker.IsAsk && !pos.Position.IsNegative()) {
				break
			}
		}
		// Maker reduce-only direction check. A reduce-only maker that
		// no longer holds an opposite position is invalid and must be
		// evicted; previously the entry was dropped but the Order
		// record + indexes leaked, leaving a phantom "open" order.
		if isPerp && best.ReduceOnly {
			pos, err := k.accountKeeper.GetPosition(ctx, best.OwnerAccountIndex, taker.MarketIndex)
			if err != nil {
				return totalFilled, perptypes.OrderStatusCancelled, err
			}
			if pos.Position.IsZero() ||
				(taker.IsAsk && !pos.Position.IsNegative()) ||
				(!taker.IsAsk && !pos.Position.IsPositive()) {
				if _, err := k.bookKeeper.EvictMakerOrder(ctx, best.OrderIndex, perptypes.OrderStatusCancelled); err != nil {
					return totalFilled, perptypes.OrderStatusCancelled, err
				}
				continue
			}
		}
		tradeBase := taker.RemainingBaseAmount
		if tradeBase > best.RemainingBaseAmount {
			tradeBase = best.RemainingBaseAmount
		}
		// Cap reduce-only fills to the taker's current position size so a
		// single trade cannot flip the account to the opposite side.
		if isPerp && taker.ReduceOnly {
			pos, err := k.accountKeeper.GetPosition(ctx, taker.OwnerAccountIndex, taker.MarketIndex)
			if err != nil {
				return totalFilled, perptypes.OrderStatusCancelled, err
			}
			limit := pos.Position.Abs().Uint64()
			if limit == 0 {
				break
			}
			if tradeBase > limit {
				tradeBase = limit
			}
		}
		// Symmetric cap on the maker side: a reduce-only maker may not
		// flip its own position. If the maker's reduce capacity is less
		// than its resting size, only fill up to that capacity and let
		// the remainder stay on the book (or get evicted by a follow-up
		// reduce-only direction check).
		if isPerp && best.ReduceOnly && tradeBase > 0 {
			pos, err := k.accountKeeper.GetPosition(ctx, best.OwnerAccountIndex, taker.MarketIndex)
			if err != nil {
				return totalFilled, perptypes.OrderStatusCancelled, err
			}
			makerLimit := pos.Position.Abs().Uint64()
			if makerLimit == 0 {
				if _, err := k.bookKeeper.EvictMakerOrder(ctx, best.OrderIndex, perptypes.OrderStatusCancelled); err != nil {
					return totalFilled, perptypes.OrderStatusCancelled, err
				}
				continue
			}
			if tradeBase > makerLimit {
				tradeBase = makerLimit
			}
		}
		if tradeBase == 0 {
			break
		}

		fill := tradekeeper.Fill{
			MakerAccountIndex: best.OwnerAccountIndex,
			TakerAccountIndex: taker.OwnerAccountIndex,
			MarketIndex:       taker.MarketIndex,
			Price:             best.Price,
			BaseAmount:        tradeBase,
			IsTakerAsk:        taker.IsAsk,
			TakerFee:          market.TakerFee,
			MakerFee:          market.MakerFee,
		}

		// Each (maker, fill) iteration runs inside an isolated cache
		// context so a recoverable failure (maker risk regression,
		// maker insufficient balance, ...) discards the partial
		// position / collateral / OI writes from ApplyPerpsMatching
		// and the matching loop can evict the bad maker and try the
		// next price level — Lighter parity with `cancel_maker_order`
		// in matching_engine.rs. A taker-side recoverable failure
		// stops the loop but preserves all prior writeCache fills.
		// Hard errors (funding settle / OI / bank failure) propagate
		// up and revert the entire CreateOrder Msg.
		sdkCtx := sdk.UnwrapSDKContext(ctx)
		cacheCtx, writeCache := sdkCtx.CacheContext()
		var applyErr error
		if isPerp {
			applyErr = k.tradeKeeper.ApplyPerpsMatching(cacheCtx, fill)
		} else {
			applyErr = k.tradeKeeper.ApplySpotMatching(cacheCtx, fill, market.BaseAssetId, market.QuoteAssetId)
		}
		if applyErr == nil {
			// FillMakerOrder also runs inside the cache so a
			// downstream failure leaves the orderbook entry
			// untouched.
			if _, err := k.bookKeeper.FillMakerOrder(cacheCtx, best.OrderIndex, tradeBase); err != nil {
				applyErr = err
			}
		}

		switch {
		case applyErr == nil:
			writeCache()
			taker.RemainingBaseAmount -= tradeBase
			totalFilled += tradeBase
			fills++
			sdkCtx.EventManager().EmitEvent(sdk.NewEvent(
				types.EventTypeOrderFill,
				sdk.NewAttribute(types.AttributeKeyMarketIndex, strconv.FormatUint(uint64(taker.MarketIndex), 10)),
				sdk.NewAttribute(types.AttributeKeyPrice, strconv.FormatUint(uint64(best.Price), 10)),
				sdk.NewAttribute(types.AttributeKeyBase, strconv.FormatUint(tradeBase, 10)),
			))
		case tradetypes.IsRecoverableMakerError(applyErr):
			// Discard cache, evict the bad maker on the OUTER ctx
			// so the next loop iteration won't peek the same
			// resting order, then continue.
			if _, err := k.bookKeeper.EvictMakerOrder(ctx, best.OrderIndex, perptypes.OrderStatusCancelled); err != nil {
				return totalFilled, perptypes.OrderStatusCancelled, err
			}
			sdkCtx.EventManager().EmitEvent(sdk.NewEvent(
				types.EventTypeMakerEvictedBadState,
				sdk.NewAttribute(types.AttributeKeyMarketIndex, strconv.FormatUint(uint64(taker.MarketIndex), 10)),
				sdk.NewAttribute(types.AttributeKeyOrderIndex, strconv.FormatUint(best.OrderIndex, 10)),
				sdk.NewAttribute(types.AttributeKeyReason, applyErr.Error()),
			))
			continue
		case tradetypes.IsRecoverableTakerError(applyErr):
			// Discard cache. Previously committed fills (any
			// writeCache calls in earlier loop iterations) are
			// retained; the residue is force-cancelled — the taker
			// just proved it cannot satisfy further fills, so
			// resting it on the book would only re-trigger the
			// same failure for downstream takers. Lighter parity:
			// `cancel_taker_order` pops the taker register
			// regardless of TIF.
			sdkCtx.EventManager().EmitEvent(sdk.NewEvent(
				types.EventTypeTakerAbortedBadState,
				sdk.NewAttribute(types.AttributeKeyMarketIndex, strconv.FormatUint(uint64(taker.MarketIndex), 10)),
				sdk.NewAttribute(types.AttributeKeyReason, applyErr.Error()),
			))
			return totalFilled, perptypes.OrderStatusCancelled, nil
		default:
			// Hard error: discard cache and propagate so the whole
			// Msg reverts atomically.
			return totalFilled, perptypes.OrderStatusCancelled, applyErr
		}
	}
	if taker.RemainingBaseAmount == 0 {
		return totalFilled, perptypes.OrderStatusFilled, nil
	}
	if totalFilled > 0 {
		return totalFilled, perptypes.OrderStatusPartiallyFilled, nil
	}
	return totalFilled, perptypes.OrderStatusOpen, nil
}
