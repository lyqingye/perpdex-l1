package keeper

import (
	"context"

	perptypes "github.com/perpdex/perpdex-l1/types"
	"github.com/perpdex/perpdex-l1/x/orderbook/types"
)

// OpenOrder accepts a freshly-created or post-match Order and reconciles
// every piece of orderbook state to match `o.Status` in one atomic step:
//
//   - Open / PartiallyFilled: build an OrderBookEntry, insert it, register
//     the client and account-open indexes, then store the Order record.
//   - Filled / Cancelled (e.g. IOC residue, zero-fill bookkeeping, or a
//     trigger-activated order that became terminal during match): persist
//     the Order and clear any pre-existing client + account-open indexes.
//     Idempotent — a brand-new terminal order with no prior indexes
//     simply no-ops on unindex.
//
// `isPostOnly` is forwarded onto `OrderBookEntry.IsPostOnly` for the resting
// path; ignored when the order is terminal.
func (k Keeper) OpenOrder(ctx context.Context, o types.Order, isPostOnly bool) error {
	switch o.Status {
	case perptypes.OrderStatusOpen, perptypes.OrderStatusPartiallyFilled:
		entry := types.OrderBookEntry{
			OrderIndex:          o.OrderIndex,
			OwnerAccountIndex:   o.OwnerAccountIndex,
			Price:               o.Price,
			Nonce:               o.Nonce,
			RemainingBaseAmount: o.RemainingBaseAmount,
			Expiry:              o.Expiry,
			IsPostOnly:          isPostOnly,
			ReduceOnly:          o.ReduceOnly,
			OrderType:           o.OrderType,
		}
		if err := k.insertOrderbookEntry(ctx, o.MarketIndex, o.IsAsk, entry); err != nil {
			return err
		}
		if err := k.indexClientOrder(ctx, o); err != nil {
			return err
		}
		if err := k.indexAccountOpenOrder(ctx, o); err != nil {
			return err
		}
	default:
		// Terminal status: clear any indexes that may have been
		// installed earlier in this order's lifetime (e.g. by
		// OpenTriggerOrder before activation). UnindexClientOrder
		// is unconditional here because the Order's own client_id
		// pair is the one we care about — UnindexClientOrderIfMatches
		// guards against cross-order re-use, but for a single-order
		// terminal transition the simple unindex is correct.
		if err := k.unindexClientOrderIfMatches(ctx, o); err != nil {
			return err
		}
		if err := k.unindexAccountOpenOrder(ctx, o); err != nil {
			return err
		}
	}
	return k.setOrder(ctx, o)
}

// OpenTriggerOrder parks a stop/take order in the trigger index until the
// mark price activates it, while still making the order discoverable via
// client_order_index and AccountOpenOrders so a cancel-all sweep finds it.
func (k Keeper) OpenTriggerOrder(ctx context.Context, o types.Order) error {
	if err := k.addTrigger(ctx, o.MarketIndex, o.TriggerPrice, o.OrderIndex); err != nil {
		return err
	}
	if err := k.indexClientOrder(ctx, o); err != nil {
		return err
	}
	if err := k.indexAccountOpenOrder(ctx, o); err != nil {
		return err
	}
	return k.setOrder(ctx, o)
}

// ActivateTrigger transitions a trigger-pending order back into an executable
// open order. It removes the trigger registration, flips Status to Open, and
// persists the Order. The caller (matching EndBlocker) is then expected to
// mutate OrderType / TimeInForce / Price for the activated variant, run the
// match loop, and call OpenOrder for any residual base.
func (k Keeper) ActivateTrigger(ctx context.Context, orderIndex uint64) (types.Order, error) {
	o, err := k.GetOrder(ctx, orderIndex)
	if err != nil {
		return types.Order{}, err
	}
	if err := k.removeTrigger(ctx, o.MarketIndex, o.TriggerPrice, o.OrderIndex); err != nil {
		return types.Order{}, err
	}
	o.Status = perptypes.OrderStatusOpen
	if err := k.setOrder(ctx, o); err != nil {
		return types.Order{}, err
	}
	return o, nil
}

// FillMakerOrder applies `filledBase` against the resting maker at
// `makerIndex`. It atomically: decrements the entry's remaining_base_amount
// (removing the entry when it reaches zero), updates the corresponding
// Order.RemainingBaseAmount and Status (PartiallyFilled or Filled), and on
// full fill clears both the client-id and account-open indexes so the
// terminal order is no longer reachable as "open". Returns the updated
// Order.
//
// `filledBase` is clamped to the maker's current remaining size; passing a
// larger value just fills the remainder rather than under/overflowing.
func (k Keeper) FillMakerOrder(ctx context.Context, makerIndex uint64, filledBase uint64) (types.Order, error) {
	maker, err := k.GetOrder(ctx, makerIndex)
	if err != nil {
		return types.Order{}, err
	}
	if filledBase > maker.RemainingBaseAmount {
		filledBase = maker.RemainingBaseAmount
	}
	if err := k.partialFill(ctx, maker.MarketIndex, maker.IsAsk, makerIndex, filledBase); err != nil {
		return types.Order{}, err
	}
	maker.RemainingBaseAmount -= filledBase
	if maker.RemainingBaseAmount == 0 {
		maker.Status = perptypes.OrderStatusFilled
		if err := k.unindexClientOrder(ctx, maker); err != nil {
			return types.Order{}, err
		}
		if err := k.unindexAccountOpenOrder(ctx, maker); err != nil {
			return types.Order{}, err
		}
	} else {
		maker.Status = perptypes.OrderStatusPartiallyFilled
	}
	if err := k.setOrder(ctx, maker); err != nil {
		return types.Order{}, err
	}
	return maker, nil
}

// EvictMakerOrder removes a maker entry mid-match (GTT expired or
// reduce-only invariant broken) and marks the underlying Order with
// `terminalStatus` (typically OrderStatusCancelled). It also clears the
// client-id and account-open indexes so the now-gone resting order does
// not survive as a stale "open" entry — fixing the historical leak where
// only the orderbook entry was removed but the Order record stayed Open
// with live indexes.
func (k Keeper) EvictMakerOrder(ctx context.Context, makerIndex uint64, terminalStatus uint32) (types.Order, error) {
	maker, err := k.GetOrder(ctx, makerIndex)
	if err != nil {
		return types.Order{}, err
	}
	if err := k.removeOrderbookEntry(ctx, maker.MarketIndex, maker.IsAsk, makerIndex); err != nil {
		return types.Order{}, err
	}
	maker.Status = terminalStatus
	if err := k.unindexClientOrderIfMatches(ctx, maker); err != nil {
		return types.Order{}, err
	}
	if err := k.unindexAccountOpenOrder(ctx, maker); err != nil {
		return types.Order{}, err
	}
	if err := k.setOrder(ctx, maker); err != nil {
		return types.Order{}, err
	}
	return maker, nil
}

// CancelOrder is the unified cancel entrypoint used by user cancels,
// liquidation cancel-all, and the orderbook GTT-expiry sweep. It branches
// on the order's current Status:
//
//   - Open / PartiallyFilled: the orderbook entry is removed.
//   - TriggeredPending:        the trigger registration is removed.
//   - anything else:           returns ErrOrderNotCancelable so callers
//     cannot accidentally overwrite a terminal Order record.
//
// In all successful branches the order's Status is set to Cancelled, the
// client-id mapping is removed only when it still points at this order
// (so a re-used client_order_index on a new order is not wiped), and the
// account-open marker is cleared. The updated Order is returned.
func (k Keeper) CancelOrder(ctx context.Context, orderIndex uint64) (types.Order, error) {
	o, err := k.GetOrder(ctx, orderIndex)
	if err != nil {
		return types.Order{}, err
	}
	switch o.Status {
	case perptypes.OrderStatusOpen, perptypes.OrderStatusPartiallyFilled:
		if o.RemainingBaseAmount == 0 {
			return types.Order{}, types.ErrOrderNotCancelable.Wrapf("order_index=%d already fully filled", o.OrderIndex)
		}
		if err := k.removeOrderbookEntry(ctx, o.MarketIndex, o.IsAsk, o.OrderIndex); err != nil {
			return types.Order{}, err
		}
	case perptypes.OrderStatusTriggeredPending:
		if err := k.removeTrigger(ctx, o.MarketIndex, o.TriggerPrice, o.OrderIndex); err != nil {
			return types.Order{}, err
		}
	default:
		return types.Order{}, types.ErrOrderNotCancelable.Wrapf("order_index=%d status=%d", o.OrderIndex, o.Status)
	}
	o.Status = perptypes.OrderStatusCancelled
	if err := k.unindexClientOrderIfMatches(ctx, o); err != nil {
		return types.Order{}, err
	}
	if err := k.unindexAccountOpenOrder(ctx, o); err != nil {
		return types.Order{}, err
	}
	if err := k.setOrder(ctx, o); err != nil {
		return types.Order{}, err
	}
	return o, nil
}
