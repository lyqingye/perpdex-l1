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
// fills) and go through the normal matching pipeline. Per-trigger failures
// are logged at error level and the sweep continues with the next trigger
// so a stale oracle / corrupt order for one market cannot jam the rest.
func (k Keeper) EndBlocker(ctx context.Context) error {
	sdkCtx := sdk.UnwrapSDKContext(ctx)
	logger := sdkCtx.Logger()
	var due []uint64
	err := k.bookKeeper.IterateTriggers(ctx, func(o orderbooktypes.Order) error {
		// Route the markPrice read through MarketKeeper so trigger
		// activation shares the fail-closed zero/staleness gate.
		// Failures are logged and the trigger is skipped; stop-loss /
		// take-profit must never fire on a stale or missing markPrice.
		markPrice, _, err := k.marketKeeper.GetMarkPriceAndDetails(ctx, o.MarketIndex)
		if err != nil {
			logger.Error(
				"matching EndBlocker: trigger oracle read failed; skipping trigger",
				"market_index", o.MarketIndex,
				"order_index", o.OrderIndex,
				"err", err,
			)
			return nil
		}
		if markPrice == 0 {
			// Defensive: GetMarkPriceAndDetails should already reject a
			// zero markPrice; guard the comparison anyway.
			return nil
		}
		// Activation semantics, mirroring the spec docs:
		//   stop-loss long (isAsk=true, protect long): trigger when markPrice <= trigger
		//   stop-loss short (isAsk=false):              trigger when markPrice >= trigger
		//   take-profit long:                           trigger when markPrice >= trigger
		//   take-profit short:                          trigger when markPrice <= trigger
		active := false
		switch o.OrderType {
		case perptypes.StopLossOrder, perptypes.StopLossLimitOrder:
			if o.IsAsk {
				active = markPrice <= o.TriggerPrice
			} else {
				active = markPrice >= o.TriggerPrice
			}
		case perptypes.TakeProfitOrder, perptypes.TakeProfitLimitOrder:
			if o.IsAsk {
				active = markPrice >= o.TriggerPrice
			} else {
				active = markPrice <= o.TriggerPrice
			}
		}
		if active {
			due = append(due, o.OrderIndex)
		}
		return nil
	})
	if err != nil {
		return err
	}

	for _, orderIndex := range due {
		o, err := k.bookKeeper.ActivateTrigger(ctx, orderIndex)
		if err != nil {
			logger.Error(
				"matching EndBlocker: ActivateTrigger failed; skipping trigger",
				"order_index", orderIndex,
				"err", err,
			)
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
		filled, status, err := k.MatchOrder(ctx, &o, params.MaxFillsPerMsg)
		_ = filled
		if err != nil {
			// Match failed mid-trigger: we already removed the
			// trigger registration via ActivateTrigger, so cancel
			// the order to keep state consistent. CancelOrder is
			// idempotent for orders without a resting entry; an
			// ErrOrderNotCancelable means the order was already
			// drained (FillMakerOrder fully consumed it) and is
			// safe to drop.
			if _, cerr := k.bookKeeper.CancelOrder(ctx, o.OrderIndex); cerr != nil {
				_ = cerr
			}
			logger.Error(
				"matching EndBlocker: post-trigger MatchOrder failed; order cancelled",
				"order_index", o.OrderIndex,
				"err", err,
			)
			continue
		}
		o.Status = status
		if o.TimeInForce == perptypes.IOC && o.RemainingBaseAmount > 0 {
			o.Status = perptypes.OrderStatusCancelled
		}
		if err := k.bookKeeper.OpenOrder(ctx, o); err != nil {
			// OpenOrder failed AFTER ActivateTrigger persisted
			// Status=Open: without cleanup the order would survive
			// as a ghost — visible via AccountOpenOrders /
			// ClientOrderIndex (installed at OpenTriggerOrder
			// time) but with no orderbook entry and no spot lock,
			// uncancelable by the user because they cannot tell
			// it ever existed. Mirror the MatchOrder error path
			// above and CancelOrder the activated order so every
			// index is dropped. CancelOrder is tolerant of
			// missing entries / locks (x/account's
			// DecreaseLockedBalance clamps), so the cleanup
			// succeeds even when applySpotLockOnOpen never
			// acquired anything.
			if _, cerr := k.bookKeeper.CancelOrder(ctx, o.OrderIndex); cerr != nil {
				_ = cerr
			}
			logger.Error(
				"matching EndBlocker: post-trigger OpenOrder failed; ghost order cancelled",
				"order_index", o.OrderIndex,
				"err", err,
			)
		}
	}
	return nil
}
