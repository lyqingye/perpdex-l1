package keeper

import (
	"context"
	"errors"

	sdk "github.com/cosmos/cosmos-sdk/types"

	perptypes "github.com/perpdex/perpdex-l1/types"
	markettypes "github.com/perpdex/perpdex-l1/x/market/types"
	orderbooktypes "github.com/perpdex/perpdex-l1/x/orderbook/types"
	tradekeeper "github.com/perpdex/perpdex-l1/x/trade/keeper"
)

// MatchOrder runs the user-driven matching loop for a CreateOrder /
// ModifyOrder taker, implementing 17-matching.md §3. Triggers,
// POST_ONLY cross detection and empty-book outcomes are handled by the
// caller (msg_server) before this function is invoked; the
// per-iteration mechanics live in match_core.go (nextMaker / matchSize
// / applyUserFill).
func (k Keeper) MatchOrder(ctx context.Context, taker *orderbooktypes.Order, maxFills uint32) (uint64, uint32, error) {
	now := sdk.UnwrapSDKContext(ctx).BlockTime().UnixMilli()
	market, err := k.marketKeeper.GetMarket(ctx, taker.MarketIndex)
	if err != nil {
		return 0, perptypes.OrderStatusCancelled, err
	}
	isPerp := market.MarketType == perptypes.MarketTypePerps

	var totalFilled uint64
	var fills uint32
	for taker.RemainingBaseAmount > 0 && fills < maxFills {
		maker, ok, err := k.nextMaker(ctx, taker, isPerp, now)
		if err != nil {
			return totalFilled, perptypes.OrderStatusCancelled, err
		}
		if !ok {
			break
		}

		base, ok, err := k.matchSize(ctx, taker, maker, isPerp)
		if err != nil {
			return totalFilled, perptypes.OrderStatusCancelled, err
		}
		if !ok {
			break
		}

		committed, err := k.applyUserFill(ctx, market, taker, maker, base)
		if errors.Is(err, errTakerRejected) {
			// Recoverable taker error: prior writeCache fills are
			// retained; the residue is force-cancelled rather than
			// rested on the book — matches `cancel_taker_order`.
			return totalFilled, perptypes.OrderStatusCancelled, nil
		}
		if err != nil {
			return totalFilled, perptypes.OrderStatusCancelled, err
		}
		if !committed {
			// Maker recoverable error: bad maker has been evicted
			// on the outer ctx. Re-peek without advancing the
			// taker residue or fill counter.
			continue
		}

		taker.RemainingBaseAmount -= base
		totalFilled += base
		fills++
		k.emitOrderFill(ctx, taker.MarketIndex, maker.Price, base)
	}

	if taker.RemainingBaseAmount == 0 {
		return totalFilled, perptypes.OrderStatusFilled, nil
	}
	if totalFilled > 0 {
		return totalFilled, perptypes.OrderStatusPartiallyFilled, nil
	}
	return totalFilled, perptypes.OrderStatusOpen, nil
}

// applyUserFill builds the appropriate Perp/Spot fill record for a
// user-driven taker and dispatches to the matching-core apply helper.
// The taker pays the market's standard maker/taker fees; liquidation
// routing fields are intentionally absent — the user path never
// carries them.
func (k Keeper) applyUserFill(
	ctx context.Context,
	market markettypes.Market,
	taker *orderbooktypes.Order,
	maker orderbooktypes.OrderBookEntry,
	base uint64,
) (bool, error) {
	if market.MarketType == perptypes.MarketTypePerps {
		return k.applyPerpFill(ctx, maker, tradekeeper.PerpFill{
			MakerAccountIndex: maker.OwnerAccountIndex,
			TakerAccountIndex: taker.OwnerAccountIndex,
			MarketIndex:       taker.MarketIndex,
			Price:             maker.Price,
			BaseAmount:        base,
			IsTakerAsk:        taker.IsAsk,
			TakerFee:          market.TakerFee,
			MakerFee:          market.MakerFee,
		})
	}
	return k.applySpotFill(ctx, maker, tradekeeper.SpotFill{
		MakerAccountIndex: maker.OwnerAccountIndex,
		TakerAccountIndex: taker.OwnerAccountIndex,
		MarketIndex:       taker.MarketIndex,
		Price:             maker.Price,
		BaseAmount:        base,
		IsTakerAsk:        taker.IsAsk,
		TakerFee:          market.TakerFee,
		MakerFee:          market.MakerFee,
	}, market.BaseAssetId, market.QuoteAssetId)
}
