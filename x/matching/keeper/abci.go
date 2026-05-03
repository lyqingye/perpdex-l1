package keeper

import (
	"context"

	sdk "github.com/cosmos/cosmos-sdk/types"

	perptypes "github.com/perpdex/perpdex-l1/types"
	orderbooktypes "github.com/perpdex/perpdex-l1/x/orderbook/types"
)

// EndBlocker iterates every order currently parked in the trigger index and
// activates those whose trigger condition has been met against the latest
// mark price. Activated triggers become resting limit orders (or IOC/market
// fills) and go through the normal matching pipeline. Errors from a single
// market are emitted as events but do not short-circuit the loop so a stale
// oracle for one market cannot jam the rest.
func (k Keeper) EndBlocker(ctx context.Context) error {
	if k.oracleKeeper == nil {
		return nil
	}
	sdkCtx := sdk.UnwrapSDKContext(ctx)
	type triggered struct {
		market       uint32
		triggerPrice uint32
		orderIndex   uint64
	}
	var due []triggered
	if err := k.bookKeeper.IterateTriggers(ctx, func(market uint32, triggerPrice uint32, orderIndex uint64) bool {
		px, err := k.oracleKeeper.GetPrice(ctx, market)
		if err != nil {
			sdkCtx.EventManager().EmitEvent(sdk.NewEvent(
				"trigger_oracle_error",
				sdk.NewAttribute("market_index", uintToStr(uint64(market))),
				sdk.NewAttribute("err", err.Error()),
			))
			return false
		}
		if px.MarkPrice == 0 {
			return false
		}
		o, err := k.bookKeeper.GetOrder(ctx, orderIndex)
		if err != nil {
			return false
		}
		// Activation semantics, mirroring lighter docs:
		//   stop-loss long (isAsk=true, protect long): trigger when mark <= trigger
		//   stop-loss short (isAsk=false):              trigger when mark >= trigger
		//   take-profit long:                           trigger when mark >= trigger
		//   take-profit short:                          trigger when mark <= trigger
		active := false
		switch o.OrderType {
		case perptypes.StopLossOrder, perptypes.StopLossLimitOrder:
			if o.IsAsk {
				active = px.MarkPrice <= triggerPrice
			} else {
				active = px.MarkPrice >= triggerPrice
			}
		case perptypes.TakeProfitOrder, perptypes.TakeProfitLimitOrder:
			if o.IsAsk {
				active = px.MarkPrice >= triggerPrice
			} else {
				active = px.MarkPrice <= triggerPrice
			}
		}
		if active {
			due = append(due, triggered{market: market, triggerPrice: triggerPrice, orderIndex: orderIndex})
		}
		return false
	}); err != nil {
		return err
	}

	for _, t := range due {
		o, err := k.bookKeeper.GetOrder(ctx, t.orderIndex)
		if err != nil {
			continue
		}
		if err := k.bookKeeper.RemoveTrigger(ctx, t.market, t.triggerPrice, t.orderIndex); err != nil {
			sdkCtx.EventManager().EmitEvent(sdk.NewEvent(
				"trigger_dequeue_error",
				sdk.NewAttribute("order_index", uintToStr(t.orderIndex)),
				sdk.NewAttribute("err", err.Error()),
			))
			continue
		}
		// Convert trigger variant to its executable twin:
		//   *_LIMIT -> LIMIT, base keeps limit price.
		//   bare STOP/TAKE -> MARKET (IOC) at zero-limit.
		switch o.OrderType {
		case perptypes.StopLossLimitOrder, perptypes.TakeProfitLimitOrder:
			o.OrderType = perptypes.LimitOrder
		default:
			o.OrderType = perptypes.MarketOrder
			o.TimeInForce = perptypes.IOC
			o.Price = 0
		}
		o.Status = perptypes.OrderStatusOpen
		params, err := k.Params.Get(ctx)
		if err != nil {
			return err
		}
		filled, status, err := k.matchOrder(ctx, &o, params.MaxFillsPerMsg)
		_ = filled
		if err != nil {
			o.Status = perptypes.OrderStatusCancelled
			_ = k.bookKeeper.SetOrder(ctx, o)
			sdkCtx.EventManager().EmitEvent(sdk.NewEvent(
				"trigger_match_error",
				sdk.NewAttribute("order_index", uintToStr(o.OrderIndex)),
				sdk.NewAttribute("err", err.Error()),
			))
			continue
		}
		o.Status = status
		if o.TimeInForce == perptypes.IOC && o.RemainingBaseAmount > 0 {
			o.Status = perptypes.OrderStatusCancelled
		} else if o.RemainingBaseAmount > 0 {
			entry := orderbooktypes.OrderBookEntry{
				OrderIndex:          o.OrderIndex,
				OwnerAccountIndex:   o.OwnerAccountIndex,
				Price:               o.Price,
				Nonce:               o.Nonce,
				RemainingBaseAmount: o.RemainingBaseAmount,
				Expiry:              o.Expiry,
				ReduceOnly:          o.ReduceOnly,
				OrderType:           o.OrderType,
			}
			if err := k.bookKeeper.InsertOrderbookEntry(ctx, o.MarketIndex, o.IsAsk, entry); err != nil {
				sdkCtx.EventManager().EmitEvent(sdk.NewEvent(
					"trigger_insert_error",
					sdk.NewAttribute("order_index", uintToStr(o.OrderIndex)),
					sdk.NewAttribute("err", err.Error()),
				))
			}
		}
		_ = k.bookKeeper.SetOrder(ctx, o)
	}
	return nil
}
