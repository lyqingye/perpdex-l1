// lifecycle.go is the orderbook keeper's atomic state-transition
// layer. Each lifecycle function is *self-contained*: its body shows
// every entry, order, index, and spot-lock side effect in one place,
// so a reviewer never has to chase the "what else changed?" question
// across multiple files.
//
// The composition rule:
//
//   - Entry-side work goes through helpers in entries.go
//     (insertEntry / removeEntry / shrinkEntryResidue).
//   - Order/index/lock work goes through helpers in orders.go
//     (indexClientOrder / indexAccountOpenOrder / addExpiryIndex /
//      applyOrderResidue / applySpotLockOnOpen / releaseSpotLockOnClose).
//   - The two layers NEVER call each other directly. Lifecycle
//     functions are the only place they are composed.
//
// This keeps the entry-residue and order-residue invariant
// (entry_drained iff order_drained) provable by local inspection of
// each lifecycle function.
package keeper

import (
	"context"

	perptypes "github.com/perpdex/perpdex-l1/types"
	"github.com/perpdex/perpdex-l1/x/orderbook/types"
)

// OpenOrder accepts a freshly-created or post-match Order and reconciles
// every piece of orderbook state to match `o.Status` in one atomic step:
//
//   - Open / PartiallyFilled: lock spot residue (no-op for perp), insert
//     the orderbook entry, register client + account-open + expiry
//     indexes, then persist the Order record.
//   - Filled / Cancelled (e.g. IOC residue, zero-fill bookkeeping, or a
//     trigger-activated order that became terminal during match): clear
//     any pre-existing indexes from a prior lifetime (OpenTriggerOrder
//     before activation, repeated terminal recording, etc.) and persist
//     the Order. Idempotent — a brand-new terminal order with no prior
//     indexes simply no-ops on every unindex call.
//
// Spot lock is acquired BEFORE the entry / index writes so that a
// failed lock (insufficient available balance) cannot leave a partially
// indexed order on the book.
func (k Keeper) OpenOrder(ctx context.Context, o types.Order) error {
	switch o.Status {
	case perptypes.OrderStatusOpen, perptypes.OrderStatusPartiallyFilled:
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
		if err := k.insertEntry(ctx, o.MarketIndex, o.IsAsk, entry); err != nil {
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
		// OpenTriggerOrder before activation). The conditional
		// unindex variant guards against cross-order client_id
		// re-use.
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
// mark price activates it. The order is also indexed for client_order_id,
// account-open reach (so cancel-all finds it), and expiry sweep (so a
// stale GTT trigger expires on schedule even before activation).
//
// No spot lock is acquired here: triggers conditionally promise a future
// trade; locking funds now would defeat their purpose for stop-market
// orders (price unknown until activation) and surprise users for
// stop-limit orders. The lock is taken at activation time inside the
// post-match OpenOrder call.
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
	if err := k.addExpiryIndex(ctx, o); err != nil {
		return err
	}
	return k.setOrder(ctx, o)
}

// ActivateTrigger transitions a trigger-pending order back into an
// executable open order. It removes the trigger registration, drops the
// pre-activation ExpiryIndex entry, flips Status to Open, and persists
// the Order. The caller (matching EndBlocker) is then expected to
// mutate OrderType / TimeInForce / Price for the activated variant,
// run the match loop, and call OpenOrder — which re-installs the
// ExpiryIndex entry for any GTT residue and clears it again on a
// terminal status.
//
// Removing the ExpiryIndex here, rather than relying on a later
// OpenOrder to overwrite the same key, makes the post-match OpenOrder
// the single authoritative owner of that index. Any path that does
// NOT reach the post-match OpenOrder (matching error, EndBlocker early
// return) therefore leaves no orphan expiry entry behind.
func (k Keeper) ActivateTrigger(ctx context.Context, orderIndex uint64) (types.Order, error) {
	o, err := k.GetOrder(ctx, orderIndex)
	if err != nil {
		return types.Order{}, err
	}
	if err := k.removeTrigger(ctx, o.MarketIndex, o.TriggerPrice, o.OrderIndex); err != nil {
		return types.Order{}, err
	}
	if err := k.removeExpiryIndex(ctx, o); err != nil {
		return types.Order{}, err
	}
	o.Status = perptypes.OrderStatusOpen
	if err := k.setOrder(ctx, o); err != nil {
		return types.Order{}, err
	}
	return o, nil
}

// FillMakerOrder applies `filledBase` against the resting maker at
// `makerIndex`. It atomically:
//
//   - shrinks the OrderBookEntry residue (and removes the entry on
//     drain) via shrinkEntryResidue
//   - shrinks the Order record residue via applyOrderResidue and
//     flips Status to Filled / PartiallyFilled accordingly
//   - on full fill clears the client_id, account-open, and expiry
//     indexes so the terminal order is no longer reachable as "open"
//
// `filledBase` is clamped (by both shrinkers, independently) to the
// current residue; passing a larger value just drains the maker.
//
// Cross-layer invariant: `entryRemoved == drained`. Both shrinkers
// clamp their own delta against their own residue copy, but because
// the Order and OrderBookEntry residues are kept in lock-step by every
// other lifecycle function, the two booleans must agree. We assert the
// invariant inline rather than trust it implicitly so any future
// regression fails loud here instead of producing a corrupt book.
func (k Keeper) FillMakerOrder(ctx context.Context, makerIndex uint64, filledBase uint64) (types.Order, error) {
	maker, err := k.GetOrder(ctx, makerIndex)
	if err != nil {
		return types.Order{}, err
	}
	if filledBase > maker.RemainingBaseAmount {
		filledBase = maker.RemainingBaseAmount
	}

	entryRemoved, _, err := k.shrinkEntryResidue(ctx, maker.MarketIndex, maker.IsAsk, makerIndex, filledBase)
	if err != nil {
		return types.Order{}, err
	}
	drained := applyOrderResidue(&maker, filledBase)
	if entryRemoved != drained {
		return types.Order{}, types.ErrInvariantViolated.Wrapf(
			"FillMakerOrder: entry_removed=%v order_drained=%v for order_index=%d (entry/order residue desync)",
			entryRemoved, drained, makerIndex,
		)
	}

	if drained {
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

// EvictMakerOrder removes a resting maker entry mid-match (GTT
// expired, reduce-only invariant broken) and marks the underlying
// Order with `terminalStatus` (typically OrderStatusCancelled).
//
// It performs the full close sequence inline so the order's exit path
// has the same shape as a user CancelOrder:
//
//   - remove the orderbook entry (entry / price-level / sort-key)
//   - release the residual spot lock (no-op for perp)
//   - clear client_id, account-open, and expiry indexes
//   - flip Status and persist the Order
//
// The pre-mutation `maker` snapshot drives releaseSpotLockOnClose so
// the released amount equals the residue that was still on the book.
func (k Keeper) EvictMakerOrder(ctx context.Context, makerIndex uint64, terminalStatus uint32) (types.Order, error) {
	maker, err := k.GetOrder(ctx, makerIndex)
	if err != nil {
		return types.Order{}, err
	}
	if err := k.removeEntry(ctx, maker.MarketIndex, maker.IsAsk, makerIndex); err != nil {
		return types.Order{}, err
	}
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
// liquidation cancel-all, the orderbook GTT-expiry sweep, and the
// matching trigger-activation cleanup path. It branches on the
// order's current Status:
//
//   - Open / PartiallyFilled: the orderbook entry is removed and the
//     residual spot lock is released.
//   - TriggeredPending:        the trigger registration is removed.
//     (No spot lock to release: triggers do not lock on placement.)
//   - anything else:           returns ErrOrderNotCancelable so
//     callers cannot accidentally overwrite a terminal Order record.
//
// In all successful branches the order's Status is set to Cancelled,
// the client_id mapping is removed only when it still points at this
// order (so a re-used client_order_index on a new order is not
// wiped), the account-open marker and the expiry index entry are
// cleared, and the updated Order record is persisted.
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
		if err := k.removeEntry(ctx, o.MarketIndex, o.IsAsk, o.OrderIndex); err != nil {
			return types.Order{}, err
		}
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
