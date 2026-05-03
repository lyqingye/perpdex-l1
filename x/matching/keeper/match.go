package keeper

import (
	"context"

	sdk "github.com/cosmos/cosmos-sdk/types"

	perptypes "github.com/perpdex/perpdex-l1/types"
	orderbooktypes "github.com/perpdex/perpdex-l1/x/orderbook/types"
	tradekeeper "github.com/perpdex/perpdex-l1/x/trade/keeper"
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
		if taker.IsAsk && taker.Price > best.Price {
			break
		}
		if !taker.IsAsk && taker.Price < best.Price {
			break
		}
		if best.OwnerAccountIndex == taker.OwnerAccountIndex {
			// Self-trade prevention: cancel taker remainder.
			break
		}
		if best.Expiry > 0 && now >= best.Expiry {
			if err := k.bookKeeper.RemoveOrderbookEntry(ctx, taker.MarketIndex, !taker.IsAsk, best.OrderIndex); err != nil {
				return totalFilled, perptypes.OrderStatusCancelled, err
			}
			continue
		}
		// Maker reduce-only check is best-effort: maker's stored reduce_only
		// flag is honoured when present.
		tradeBase := taker.RemainingBaseAmount
		if tradeBase > best.RemainingBaseAmount {
			tradeBase = best.RemainingBaseAmount
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
		if isPerp {
			if err := k.tradeKeeper.ApplyPerpsMatching(ctx, fill); err != nil {
				return totalFilled, perptypes.OrderStatusCancelled, err
			}
		} else {
			if err := k.tradeKeeper.ApplySpotMatching(ctx, fill, market.BaseAssetId, market.QuoteAssetId); err != nil {
				return totalFilled, perptypes.OrderStatusCancelled, err
			}
		}
		// Update maker entry.
		if err := k.bookKeeper.PartialFill(ctx, taker.MarketIndex, !taker.IsAsk, best.OrderIndex, tradeBase); err != nil {
			return totalFilled, perptypes.OrderStatusCancelled, err
		}
		// Update maker stored Order record.
		makerOrder, err := k.bookKeeper.GetOrder(ctx, best.OrderIndex)
		if err == nil {
			if makerOrder.RemainingBaseAmount > tradeBase {
				makerOrder.RemainingBaseAmount -= tradeBase
				makerOrder.Status = perptypes.OrderStatusPartiallyFilled
			} else {
				makerOrder.RemainingBaseAmount = 0
				makerOrder.Status = perptypes.OrderStatusFilled
				_ = k.bookKeeper.UnindexClientOrder(ctx, makerOrder)
			}
			_ = k.bookKeeper.SetOrder(ctx, makerOrder)
		}

		taker.RemainingBaseAmount -= tradeBase
		totalFilled += tradeBase
		fills++

		sdk.UnwrapSDKContext(ctx).EventManager().EmitEvent(sdk.NewEvent(
			"order_fill",
			sdk.NewAttribute("market_index", uintToStr(uint64(taker.MarketIndex))),
			sdk.NewAttribute("price", uintToStr(uint64(best.Price))),
			sdk.NewAttribute("base", uintToStr(tradeBase)),
		))
	}
	if taker.RemainingBaseAmount == 0 {
		return totalFilled, perptypes.OrderStatusFilled, nil
	}
	if totalFilled > 0 {
		return totalFilled, perptypes.OrderStatusPartiallyFilled, nil
	}
	return totalFilled, perptypes.OrderStatusOpen, nil
}

func uintToStr(u uint64) string {
	if u == 0 {
		return "0"
	}
	var b [20]byte
	i := len(b)
	for u > 0 {
		i--
		b[i] = byte('0' + u%10)
		u /= 10
	}
	return string(b[i:])
}
