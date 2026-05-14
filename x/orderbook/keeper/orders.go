// orders.go is the orderbook keeper's "order + index" layer. It owns
// Order-record CRUD and every secondary index that points at orders:
//
//   - UserOrderIndex       (market, account, client_order_index)
//   - AccountOpenOrders    (account, market, order_index)
//   - AccountOpenOrderCount(account, market)
//   - TriggerIndex         (market, trigger_price, order_index)
//   - ExpiryIndex          (expiry_ms, order_index)
//
// Plus the per-Order spot lock helpers (computeSpotLock /
// applySpotLockOnOpen / releaseSpotLockOnClose) — these read Order
// fields and the market metadata, so they belong with the order layer
// rather than the entry layer.
//
// This file is intentionally OrderBookEntry-agnostic. Lifecycle
// operations (lifecycle.go) compose this layer with the entry layer
// (entries.go) by calling both explicitly so the dual-write is
// visible at every state transition.
package keeper

import (
	"context"
	"errors"
	stdmath "math"

	"cosmossdk.io/collections"
	"cosmossdk.io/math"

	perptypes "github.com/perpdex/perpdex-l1/types"
	markettypes "github.com/perpdex/perpdex-l1/x/market/types"
	"github.com/perpdex/perpdex-l1/x/orderbook/types"
)

// applyOrderResidue is a pure helper that subtracts `delta` from the
// order's remaining base (clamped to the current value) and reports
// whether the residue reached zero. Lifecycle callers pair this with
// `Keeper.shrinkEntryResidue` from entries.go; the boolean return is
// the cross-layer invariant check (entry-removed iff order-drained).
func applyOrderResidue(o *types.Order, delta uint64) (drained bool) {
	if delta > o.RemainingBaseAmount {
		delta = o.RemainingBaseAmount
	}
	o.RemainingBaseAmount -= delta
	return o.RemainingBaseAmount == 0
}

// --- client-order index ----------------------------------------------------

// indexClientOrder records the (market, account, client_order_index) ->
// order_index mapping. No-op when `client_order_index == 0`.
//
// Internal helper. External callers go through OpenOrder /
// OpenTriggerOrder in lifecycle.go.
func (k Keeper) indexClientOrder(ctx context.Context, o types.Order) error {
	if o.ClientOrderIndex == 0 {
		return nil
	}
	return k.UserOrderIndex.Set(ctx, collections.Join3(o.MarketIndex, o.OwnerAccountIndex, o.ClientOrderIndex), o.OrderIndex)
}

// unindexClientOrder unconditionally removes the (market, account,
// client_order_index) -> order mapping. Used by FillMakerOrder on full
// fill: at that point we know the row points at the maker we just
// drained (no other writer races to install a competing pointer).
//
// Internal helper.
func (k Keeper) unindexClientOrder(ctx context.Context, o types.Order) error {
	if o.ClientOrderIndex == 0 {
		return nil
	}
	return k.UserOrderIndex.Remove(ctx, collections.Join3(o.MarketIndex, o.OwnerAccountIndex, o.ClientOrderIndex))
}

// unindexClientOrderIfMatches is the conditional unindex used by
// Cancel / Evict so a stale order's cleanup cannot accidentally
// delete the mapping for a newer order that re-used the same
// client_order_index.
//
// Internal helper.
func (k Keeper) unindexClientOrderIfMatches(ctx context.Context, o types.Order) error {
	if o.ClientOrderIndex == 0 {
		return nil
	}
	key := collections.Join3(o.MarketIndex, o.OwnerAccountIndex, o.ClientOrderIndex)
	cur, err := k.UserOrderIndex.Get(ctx, key)
	if err != nil {
		if errors.Is(err, collections.ErrNotFound) {
			return nil
		}
		return err
	}
	if cur != o.OrderIndex {
		return nil
	}
	return k.UserOrderIndex.Remove(ctx, key)
}

// HasOpenClientOrder returns (true, orderIndex) when the (market,
// account, clientID) tuple currently maps to an order in a non-terminal
// status (Open / PartiallyFilled / TriggeredPending). Returns
// (false, 0, nil) otherwise; never returns ErrNotFound to the caller.
func (k Keeper) HasOpenClientOrder(ctx context.Context, market uint32, account uint64, clientID uint64) (bool, uint64, error) {
	if clientID == 0 {
		return false, 0, nil
	}
	idx, err := k.UserOrderIndex.Get(ctx, collections.Join3(market, account, clientID))
	if err != nil {
		if errors.Is(err, collections.ErrNotFound) {
			return false, 0, nil
		}
		return false, 0, err
	}
	o, err := k.GetOrder(ctx, idx)
	if err != nil {
		if errors.Is(err, types.ErrOrderNotFound) {
			return false, 0, nil
		}
		return false, 0, err
	}
	switch o.Status {
	case perptypes.OrderStatusOpen,
		perptypes.OrderStatusPartiallyFilled,
		perptypes.OrderStatusTriggeredPending:
		return true, idx, nil
	}
	return false, 0, nil
}

// --- account-open index ---------------------------------------------------

// accountOpenOrderKey assembles the (account, market, order_index) triple
// used by AccountOpenOrders. The leading account prefix powers the
// `IterateAccountOpenOrders` scan; the embedded market lets cancel-all
// filter by market without loading every Order.
func accountOpenOrderKey(o types.Order) collections.Triple[uint64, uint32, uint64] {
	return collections.Join3(o.OwnerAccountIndex, o.MarketIndex, o.OrderIndex)
}

// indexAccountOpenOrder marks `o` as a non-terminal order owned by
// `o.OwnerAccountIndex`. Independent of client_order_index so cancel-all
// can find every resting order.
//
// Internal helper. External callers go through OpenOrder /
// OpenTriggerOrder in lifecycle.go.
func (k Keeper) indexAccountOpenOrder(ctx context.Context, o types.Order) error {
	key := accountOpenOrderKey(o)
	had, err := k.AccountOpenOrders.Has(ctx, key)
	if err != nil {
		return err
	}
	if err := k.AccountOpenOrders.Set(ctx, key); err != nil {
		return err
	}
	if had {
		// Already counted (e.g. ActivateTrigger -> OpenOrder
		// re-indexes the same order). Avoid double-incrementing
		// the per-(account,market) cap counter.
		return nil
	}
	return k.bumpAccountOpenOrderCount(ctx, o.OwnerAccountIndex, o.MarketIndex, +1)
}

// unindexAccountOpenOrder removes the (account, market, order_index)
// triple. Safe to call on a tuple that was never indexed.
//
// Internal helper.
func (k Keeper) unindexAccountOpenOrder(ctx context.Context, o types.Order) error {
	key := accountOpenOrderKey(o)
	had, err := k.AccountOpenOrders.Has(ctx, key)
	if err != nil {
		return err
	}
	if err := k.AccountOpenOrders.Remove(ctx, key); err != nil {
		return err
	}
	if !had {
		return nil
	}
	return k.bumpAccountOpenOrderCount(ctx, o.OwnerAccountIndex, o.MarketIndex, -1)
}

// bumpAccountOpenOrderCount adjusts the cap-tracking counter by delta
// (+1 / -1). The runtime path is strict — a negative result is reported
// as `ErrInvariantViolated` because indexAccountOpenOrder /
// unindexAccountOpenOrder are the only writers and they take the
// "AccountOpenOrders.Has(...)" pre-check before touching the counter,
// so a drift here means the keyset and the counter went out of sync.
//
// Genesis rehydration calls indexAccountOpenOrder on a fresh keyset so
// it only increments; the strict guard never fires from genesis paths.
func (k Keeper) bumpAccountOpenOrderCount(ctx context.Context, accIdx uint64, marketIdx uint32, delta int32) error {
	key := collections.Join(accIdx, marketIdx)
	cur, err := k.AccountOpenOrderCount.Get(ctx, key)
	if err != nil && !errors.Is(err, collections.ErrNotFound) {
		return err
	}
	next := int64(cur) + int64(delta)
	if next < 0 {
		return types.ErrInvariantViolated.Wrapf(
			"account_open_order_count under-subtract: account=%d market=%d cur=%d delta=%d",
			accIdx, marketIdx, cur, delta,
		)
	}
	if next > stdmath.MaxUint32 {
		return types.ErrInvariantViolated.Wrapf(
			"account_open_order_count overflow: account=%d market=%d cur=%d delta=%d",
			accIdx, marketIdx, cur, delta,
		)
	}
	if next == 0 {
		if cur != 0 {
			return k.AccountOpenOrderCount.Remove(ctx, key)
		}
		return nil
	}
	return k.AccountOpenOrderCount.Set(ctx, key, uint32(next))
}

// GetAccountOpenOrderCount returns the current number of resting +
// trigger-pending orders held by `account` in `market`. Zero is
// returned for an absent row, matching the implicit default.
func (k Keeper) GetAccountOpenOrderCount(ctx context.Context, accIdx uint64, marketIdx uint32) (uint32, error) {
	cur, err := k.AccountOpenOrderCount.Get(ctx, collections.Join(accIdx, marketIdx))
	if err != nil {
		if errors.Is(err, collections.ErrNotFound) {
			return 0, nil
		}
		return 0, err
	}
	return cur, nil
}

// IterateAccountOpenOrders walks every order currently indexed as open
// for `account`. When `marketFilter != 0` the iterator is scoped to
// that market via a (account, market) double-prefix range — orders in
// other markets are skipped at the key layer, not via per-order
// GetOrder + post-filter.
//
// Callback contract:
//   - return `nil`                          to continue
//   - return `types.ErrStopIteration`       to terminate cleanly
//   - return any other non-nil error        to abort iteration; the
//     error is propagated verbatim to the caller (no closure-captured
//     error variable needed)
//   - the callback MUST NOT mutate AccountOpenOrders (no CancelOrder /
//     OpenOrder / EvictMakerOrder / OpenTriggerOrder) — collect target
//     order indexes into a local slice and process them AFTER this
//     method returns. The canonical pattern is in
//     x/matching/keeper/msg_server.go::CancelAllOrders.
func (k Keeper) IterateAccountOpenOrders(
	ctx context.Context,
	account uint64,
	marketFilter uint32,
	cb func(types.Order) error,
) error {
	var rng collections.Ranger[collections.Triple[uint64, uint32, uint64]]
	if marketFilter == 0 {
		rng = collections.NewPrefixedTripleRange[uint64, uint32, uint64](account)
	} else {
		rng = collections.NewSuperPrefixedTripleRange[uint64, uint32, uint64](account, marketFilter)
	}
	iter, err := k.AccountOpenOrders.Iterate(ctx, rng)
	if err != nil {
		return err
	}
	defer iter.Close()
	for ; iter.Valid(); iter.Next() {
		key, err := iter.Key()
		if err != nil {
			return err
		}
		o, err := k.GetOrder(ctx, key.K3())
		if err != nil {
			if errors.Is(err, types.ErrOrderNotFound) {
				continue
			}
			return err
		}
		if cbErr := cb(o); cbErr != nil {
			if errors.Is(cbErr, types.ErrStopIteration) {
				return nil
			}
			return cbErr
		}
	}
	return nil
}

// --- trigger index --------------------------------------------------------

// addTrigger registers an order with a trigger price.
//
// Internal helper.
func (k Keeper) addTrigger(ctx context.Context, market uint32, triggerPrice uint32, orderIndex uint64) error {
	return k.TriggerIndex.Set(ctx, collections.Join3(market, triggerPrice, orderIndex))
}

// removeTrigger drops the trigger entry for an order.
//
// Internal helper.
func (k Keeper) removeTrigger(ctx context.Context, market uint32, triggerPrice uint32, orderIndex uint64) error {
	return k.TriggerIndex.Remove(ctx, collections.Join3(market, triggerPrice, orderIndex))
}

// IterateTriggers walks every order currently parked in the trigger
// index in ascending (market, trigger_price, order_index) order. The
// keeper loads the underlying Order so the callback receives the full
// record directly — call sites no longer need a second GetOrder step
// and can read market / trigger price off the Order itself.
//
// Callback contract:
//   - return `nil`                    to continue
//   - return `types.ErrStopIteration` to terminate cleanly
//   - any other non-nil error aborts iteration and is propagated
//
// The Order's MarketIndex always equals the index entry's market key,
// and o.TriggerPrice equals the index entry's trigger_price key (the
// trigger index is keyed by `o.TriggerPrice` at OpenTriggerOrder time
// and the field is not mutated before ActivateTrigger removes the
// entry). A missing Order record for an indexed entry is skipped
// without error to keep the iterator robust against unrelated
// genesis/migration anomalies.
func (k Keeper) IterateTriggers(ctx context.Context, cb func(types.Order) error) error {
	iter, err := k.TriggerIndex.Iterate(ctx, nil)
	if err != nil {
		return err
	}
	defer iter.Close()
	for ; iter.Valid(); iter.Next() {
		key, err := iter.Key()
		if err != nil {
			return err
		}
		o, err := k.GetOrder(ctx, key.K3())
		if err != nil {
			if errors.Is(err, types.ErrOrderNotFound) {
				continue
			}
			return err
		}
		if cbErr := cb(o); cbErr != nil {
			if errors.Is(cbErr, types.ErrStopIteration) {
				return nil
			}
			return cbErr
		}
	}
	return nil
}

// --- expiry index ---------------------------------------------------------

// addExpiryIndex registers `o` in the GTT expiry index so EndBlocker
// can walk only the orders due to expire by `now`. Non-GTT orders and
// GTT orders with `Expiry == 0` are not indexed (they never expire).
//
// Internal helper.
func (k Keeper) addExpiryIndex(ctx context.Context, o types.Order) error {
	if !indexExpiry(o) {
		return nil
	}
	return k.ExpiryIndex.Set(ctx, collections.Join(o.Expiry, o.OrderIndex))
}

// removeExpiryIndex drops the (expiry, order_index) entry. Safe to call
// on orders that were never indexed (Expiry==0 short-circuits, missing
// keys are tolerated as no-ops).
//
// The gate is intentionally weaker than `indexExpiry`: a trigger order
// added under TimeInForce==GTT can have its TIF rewritten to IOC by
// matching when the activated variant becomes a market order, so the
// keyset entry must still be cleaned up even though the post-mutation
// order would not satisfy `indexExpiry`. The key uses only (Expiry,
// OrderIndex), both of which survive the rewrite.
func (k Keeper) removeExpiryIndex(ctx context.Context, o types.Order) error {
	if o.Expiry == 0 {
		return nil
	}
	err := k.ExpiryIndex.Remove(ctx, collections.Join(o.Expiry, o.OrderIndex))
	if err != nil && !errors.Is(err, collections.ErrNotFound) {
		return err
	}
	return nil
}

// indexExpiry reports whether `o` should appear in the GTT expiry
// keyset. An order is indexed iff TimeInForce==GTT and Expiry > 0; the
// EndBlocker walks the keyset by ascending expiry timestamp so each
// block does O(due_orders) work instead of O(N_history).
func indexExpiry(o types.Order) bool {
	return o.TimeInForce == perptypes.GTT && o.Expiry > 0
}

// IsExpired reports whether the order has passed its expiry.
func IsExpired(o types.Order, now int64) bool {
	if o.TimeInForce != perptypes.GTT {
		return false
	}
	return o.Expiry > 0 && now >= o.Expiry
}

// --- spot lock helpers ----------------------------------------------------

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
