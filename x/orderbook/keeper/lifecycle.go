package keeper

import (
	"context"

	"cosmossdk.io/math"

	perptypes "github.com/perpdex/perpdex-l1/types"
	markettypes "github.com/perpdex/perpdex-l1/x/market/types"
	"github.com/perpdex/perpdex-l1/x/orderbook/types"
)

// computeSpotLock returns (assetID, amount) the order should hold while
// resting on the orderbook. For an ask the seller locks `remaining_base`
// units of the base asset; for a bid the buyer locks `remaining_base *
// price` units of the quote asset, mirroring
// `get_locked_amount_and_ask_asset_index` in l2_create_order.rs.
func computeSpotLock(o types.Order, market markettypes.Market) (uint32, math.Int) {
	if o.IsAsk {
		return market.BaseAssetId, math.NewIntFromUint64(o.RemainingBaseAmount)
	}
	notional := math.NewIntFromUint64(o.RemainingBaseAmount).
		Mul(math.NewIntFromUint64(uint64(o.Price)))
	return market.QuoteAssetId, notional
}

// applySpotLockOnOpen reserves the proportional resources for a spot
// resting order. Perp markets are no-ops: their resource control comes
// from the open-order count cap (and the post-trade risk check), not a
// real balance lock.
func (k Keeper) applySpotLockOnOpen(ctx context.Context, o types.Order) error {
	if k.spotLocker == nil {
		return nil
	}
	market, err := k.marketKeeper.GetMarket(ctx, o.MarketIndex)
	if err != nil {
		return err
	}
	if market.MarketType != perptypes.MarketTypeSpot {
		return nil
	}
	assetID, amount := computeSpotLock(o, market)
	return k.spotLocker.IncreaseLockedBalance(ctx, o.OwnerAccountIndex, assetID, amount)
}

// releaseSpotLockOnClose drops the (still-locked) portion of a resting
// spot order's resources when it leaves the book without trading further
// (cancel / GTT eviction / reduce-only eviction). For a partially-filled
// order, `o.RemainingBaseAmount` already reflects the post-fill residue,
// which equals the residual lock — partial fills consume the lock 1:1
// inside ApplySpotMatching's spotMakerDebit, so we only release what is
// still reserved here.
func (k Keeper) releaseSpotLockOnClose(ctx context.Context, o types.Order) error {
	if k.spotLocker == nil {
		return nil
	}
	market, err := k.marketKeeper.GetMarket(ctx, o.MarketIndex)
	if err != nil {
		return err
	}
	if market.MarketType != perptypes.MarketTypeSpot {
		return nil
	}
	assetID, amount := computeSpotLock(o, market)
	return k.spotLocker.DecreaseLockedBalance(ctx, o.OwnerAccountIndex, assetID, amount)
}

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
func (k Keeper) OpenOrder(ctx context.Context, o types.Order) error {
	switch o.Status {
	case perptypes.OrderStatusOpen, perptypes.OrderStatusPartiallyFilled:
		// Lock-on-place for spot residue first: if the lock fails
		// (insufficient available balance) the entry/index writes
		// below never happen, so a malicious caller cannot rest an
		// under-funded spot order. Perp markets are no-ops here.
		if err := k.applySpotLockOnOpen(ctx, o); err != nil {
			return err
		}
		entry := types.OrderBookEntry{
			OrderIndex:          o.OrderIndex,
			OwnerAccountIndex:   o.OwnerAccountIndex,
			Price:               o.Price,
			Nonce:               o.Nonce,
			RemainingBaseAmount: o.RemainingBaseAmount,
			Expiry:              o.Expiry,
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
		if err := k.addExpiryIndex(ctx, o); err != nil {
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
		if err := k.removeExpiryIndex(ctx, o); err != nil {
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
	// Trigger-pending GTT orders must expire on schedule even before
	// they activate. Indexing here lets EndBlocker cancel a stale
	// stop-loss without having to scan the trigger keyset every
	// block.
	if err := k.addExpiryIndex(ctx, o); err != nil {
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
		if err := k.removeExpiryIndex(ctx, maker); err != nil {
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
// not survive as a stale "open" entry.
func (k Keeper) EvictMakerOrder(ctx context.Context, makerIndex uint64, terminalStatus uint32) (types.Order, error) {
	maker, err := k.GetOrder(ctx, makerIndex)
	if err != nil {
		return types.Order{}, err
	}
	if err := k.removeOrderbookEntry(ctx, maker.MarketIndex, maker.IsAsk, makerIndex); err != nil {
		return types.Order{}, err
	}
	// Release any still-locked spot resources before flipping the order
	// to its terminal status. We use the pre-mutation copy of `maker`
	// (with its current RemainingBaseAmount) because the lock that was
	// reserved at OpenOrder time is sized off the residue.
	if err := k.releaseSpotLockOnClose(ctx, maker); err != nil {
		return types.Order{}, err
	}
	maker.Status = terminalStatus
	if err := k.unindexClientOrderIfMatches(ctx, maker); err != nil {
		return types.Order{}, err
	}
	if err := k.unindexAccountOpenOrder(ctx, maker); err != nil {
		return types.Order{}, err
	}
	if err := k.removeExpiryIndex(ctx, maker); err != nil {
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
		// Release residue lock for resting spot orders. Trigger
		// cancels do not need to release because triggers do not
		// lock at OpenTriggerOrder time — the lock only happens
		// after activation when the order rests on the book.
		if err := k.releaseSpotLockOnClose(ctx, o); err != nil {
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
	if err := k.removeExpiryIndex(ctx, o); err != nil {
		return types.Order{}, err
	}
	if err := k.setOrder(ctx, o); err != nil {
		return types.Order{}, err
	}
	return o, nil
}
