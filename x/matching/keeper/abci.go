package keeper

import (
	"context"
	"strconv"

	sdk "github.com/cosmos/cosmos-sdk/types"

	perptypes "github.com/perpdex/perpdex-l1/types"
	"github.com/perpdex/perpdex-l1/x/matching/types"
)

// EndBlocker iterates every order currently parked in the trigger index and
// activates those whose trigger condition has been met against the latest
// mark price. Activated triggers become resting limit orders (or IOC/market
// fills) and go through the normal matching pipeline. Errors from a single
// market are emitted as events but do not short-circuit the loop so a stale
// oracle for one market cannot jam the rest.
func (k Keeper) EndBlocker(ctx context.Context) error {
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
				types.EventTypeTriggerOracleError,
				sdk.NewAttribute(types.AttributeKeyMarketIndex, strconv.FormatUint(uint64(market), 10)),
				sdk.NewAttribute(types.AttributeKeyErr, err.Error()),
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
		// Activation semantics, mirroring the spec docs:
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
		o, err := k.bookKeeper.ActivateTrigger(ctx, t.orderIndex)
		if err != nil {
			sdkCtx.EventManager().EmitEvent(sdk.NewEvent(
				types.EventTypeTriggerDequeueError,
				sdk.NewAttribute(types.AttributeKeyOrderIndex, strconv.FormatUint(t.orderIndex, 10)),
				sdk.NewAttribute(types.AttributeKeyErr, err.Error()),
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
		params, err := k.Params.Get(ctx)
		if err != nil {
			return err
		}
		filled, status, err := k.matchOrder(ctx, &o, params.MaxFillsPerMsg)
		_ = filled
		if err != nil {
			// Match failed mid-trigger: we already removed the
			// trigger registration via ActivateTrigger, so cancel
			// the order to keep state consistent. CancelOrder is
			// idempotent for orders without a resting entry.
			if _, cerr := k.bookKeeper.CancelOrder(ctx, o.OrderIndex); cerr != nil {
				// Already-terminal orders or missing entries
				// surface as ErrOrderNotCancelable; ignore so
				// the original match error wins.
				_ = cerr
			}
			sdkCtx.EventManager().EmitEvent(sdk.NewEvent(
				types.EventTypeTriggerMatchError,
				sdk.NewAttribute(types.AttributeKeyOrderIndex, strconv.FormatUint(o.OrderIndex, 10)),
				sdk.NewAttribute(types.AttributeKeyErr, err.Error()),
			))
			continue
		}
		o.Status = status
		if o.TimeInForce == perptypes.IOC && o.RemainingBaseAmount > 0 {
			o.Status = perptypes.OrderStatusCancelled
		}
		if err := k.bookKeeper.OpenOrder(ctx, o, false); err != nil {
			sdkCtx.EventManager().EmitEvent(sdk.NewEvent(
				types.EventTypeTriggerInsertError,
				sdk.NewAttribute(types.AttributeKeyOrderIndex, strconv.FormatUint(o.OrderIndex, 10)),
				sdk.NewAttribute(types.AttributeKeyErr, err.Error()),
			))
		}
	}
	return nil
}
