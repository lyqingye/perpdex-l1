package keeper

import (
	"context"
	"errors"
	"math/big"

	"cosmossdk.io/collections"
	"cosmossdk.io/math"

	perptypes "github.com/perpdex/perpdex-l1/types"
	"github.com/perpdex/perpdex-l1/x/orderbook/types"
)

// sideByte returns the discriminator byte used as the first byte of the
// orderbook entry sort-key (ask=0, bid=1) so each side iterates separately.
func sideByte(isAsk bool) byte {
	if isAsk {
		return 0
	}
	return 1
}

// composeSortKey returns sideByte || sortableKey (13 bytes).
func composeSortKey(isAsk bool, sk []byte) []byte {
	out := make([]byte, 0, 1+len(sk))
	out = append(out, sideByte(isAsk))
	out = append(out, sk...)
	return out
}

// sidePrefix returns the 1-byte prefix used to iterate one side of a market.
func sidePrefix(isAsk bool) []byte { return []byte{sideByte(isAsk)} }

// AllocateOrderIndex returns the next order_index, ensuring it starts at 1.
func (k Keeper) AllocateOrderIndex(ctx context.Context) (uint64, error) {
	idx, err := k.NextOrderIndex.Next(ctx)
	if err != nil {
		return 0, err
	}
	if idx == 0 {
		idx, err = k.NextOrderIndex.Next(ctx)
		if err != nil {
			return 0, err
		}
	}
	return idx, nil
}

// insertOrderbookEntry adds an order to the orderbook (sorted side store) and
// updates the price-level aggregate. The per-entry quote notional is capped
// by `perptypes.MaxOrderQuoteAmount` (~2.8e14) and the price-level aggregate
// is guarded against uint64 overflow in `adjustPriceLevel`.
//
// Internal helper. External callers go through OpenOrder.
func (k Keeper) insertOrderbookEntry(ctx context.Context, market uint32, isAsk bool, o types.OrderBookEntry) error {
	quote, err := checkedQuote(o.RemainingBaseAmount, uint64(o.Price))
	if err != nil {
		return err
	}
	sk := types.SortableKey(o.Price, o.Nonce, isAsk)
	composed := composeSortKey(isAsk, sk)
	if err := k.OrderBookEntries.Set(ctx, collections.Join(market, composed), o); err != nil {
		return err
	}
	if err := k.OrderToSortKey.Set(ctx, collections.Join(market, o.OrderIndex), composed); err != nil {
		return err
	}
	return k.adjustPriceLevel(ctx, market, isAsk, o.Price, int64(o.RemainingBaseAmount), int64(quote), 1)
}

// removeOrderbookEntry deletes the entry and decrements the price-level.
//
// Internal helper. External callers go through CancelOrder / EvictMakerOrder.
func (k Keeper) removeOrderbookEntry(ctx context.Context, market uint32, isAsk bool, orderIndex uint64) error {
	composed, err := k.OrderToSortKey.Get(ctx, collections.Join(market, orderIndex))
	if err != nil {
		if errors.Is(err, collections.ErrNotFound) {
			return nil
		}
		return err
	}
	entry, err := k.OrderBookEntries.Get(ctx, collections.Join(market, composed))
	if err != nil {
		if errors.Is(err, collections.ErrNotFound) {
			return nil
		}
		return err
	}
	if err := k.OrderBookEntries.Remove(ctx, collections.Join(market, composed)); err != nil {
		return err
	}
	if err := k.OrderToSortKey.Remove(ctx, collections.Join(market, orderIndex)); err != nil {
		return err
	}
	// Remove the notional contribution computed the same way the insert
	// added it. Entries that survive have already passed the quote cap
	// on insert so the multiply never overflows here.
	quote, err := checkedQuote(entry.RemainingBaseAmount, uint64(entry.Price))
	if err != nil {
		return err
	}
	return k.adjustPriceLevel(ctx, market, isAsk, entry.Price,
		-int64(entry.RemainingBaseAmount),
		-int64(quote),
		-1,
	)
}

// partialFill subtracts filledBase from the remaining_base_amount of the entry
// and updates the price level. If remaining drops to 0 the entry is removed.
//
// Internal helper. External callers go through FillMakerOrder.
func (k Keeper) partialFill(ctx context.Context, market uint32, isAsk bool, orderIndex uint64, filledBase uint64) error {
	composed, err := k.OrderToSortKey.Get(ctx, collections.Join(market, orderIndex))
	if err != nil {
		return err
	}
	tripKey := collections.Join(market, composed)
	entry, err := k.OrderBookEntries.Get(ctx, tripKey)
	if err != nil {
		return err
	}
	if filledBase >= entry.RemainingBaseAmount {
		filledBase = entry.RemainingBaseAmount
	}
	entry.RemainingBaseAmount -= filledBase
	// Compute the quote delta using checked multiplication so a corrupt
	// entry or a future cap change cannot silently overflow.
	filledQuote, err := checkedQuote(filledBase, uint64(entry.Price))
	if err != nil {
		return err
	}
	if err := k.adjustPriceLevel(ctx, market, isAsk, entry.Price,
		-int64(filledBase),
		-int64(filledQuote),
		0,
	); err != nil {
		return err
	}
	if entry.RemainingBaseAmount == 0 {
		if err := k.OrderBookEntries.Remove(ctx, tripKey); err != nil {
			return err
		}
		if err := k.OrderToSortKey.Remove(ctx, collections.Join(market, orderIndex)); err != nil {
			return err
		}
		return k.adjustPriceLevel(ctx, market, isAsk, entry.Price, 0, 0, -1)
	}
	return k.OrderBookEntries.Set(ctx, tripKey, entry)
}

// PeekBestOpposite returns the head-of-book entry on the opposite side of
// `isAsk`. Returns (entry, true, nil) if an order exists.
func (k Keeper) PeekBestOpposite(ctx context.Context, market uint32, isAsk bool) (types.OrderBookEntry, bool, error) {
	prefix := sidePrefix(!isAsk)
	rng := new(collections.Range[collections.Pair[uint32, []byte]]).
		Prefix(collections.PairPrefix[uint32, []byte](market))
	iter, err := k.OrderBookEntries.Iterate(ctx, rng)
	if err != nil {
		return types.OrderBookEntry{}, false, err
	}
	defer iter.Close()
	for ; iter.Valid(); iter.Next() {
		k2, err := iter.Key()
		if err != nil {
			return types.OrderBookEntry{}, false, err
		}
		composed := k2.K2()
		if len(composed) == 0 || composed[0] != prefix[0] {
			continue
		}
		v, err := iter.Value()
		if err != nil {
			return types.OrderBookEntry{}, false, err
		}
		return v, true, nil
	}
	return types.OrderBookEntry{}, false, nil
}

// WouldCross reports whether placing an order at `price` on `isAsk` side would
// match against the best opposite order.
func (k Keeper) WouldCross(ctx context.Context, market uint32, isAsk bool, price uint32) (bool, error) {
	best, ok, err := k.PeekBestOpposite(ctx, market, isAsk)
	if err != nil || !ok {
		return false, err
	}
	if isAsk {
		return price <= best.Price, nil
	}
	return price >= best.Price, nil
}

// adjustPriceLevel applies signed deltas to the price-level aggregate at
// (market, price) for the given side. The quote aggregate is guarded against
// uint64 overflow (returning ErrPriceLevelOverflow), so a burst of large
// orders at the same level cannot silently wrap around.
func (k Keeper) adjustPriceLevel(ctx context.Context, market uint32, isAsk bool, price uint32, baseDelta, quoteDelta int64, countDelta int32) error {
	key := collections.Join(market, price)
	pl, err := k.PriceLevels.Get(ctx, key)
	if err != nil && !errors.Is(err, collections.ErrNotFound) {
		return err
	}
	if errors.Is(err, collections.ErrNotFound) {
		pl = types.PriceLevelAggregate{MarketIndex: market, Price: price}
	}
	if isAsk {
		pl.AskBaseSum = applyDelta(pl.AskBaseSum, baseDelta)
		q, err := applyQuoteDelta(pl.AskQuoteSum, quoteDelta)
		if err != nil {
			return err
		}
		pl.AskQuoteSum = q
		pl.AskCount = uint32(int32(pl.AskCount) + countDelta)
	} else {
		pl.BidBaseSum = applyDelta(pl.BidBaseSum, baseDelta)
		q, err := applyQuoteDelta(pl.BidQuoteSum, quoteDelta)
		if err != nil {
			return err
		}
		pl.BidQuoteSum = q
		pl.BidCount = uint32(int32(pl.BidCount) + countDelta)
	}
	if pl.AskBaseSum == 0 && pl.BidBaseSum == 0 && pl.AskCount == 0 && pl.BidCount == 0 {
		return k.PriceLevels.Remove(ctx, key)
	}
	return k.PriceLevels.Set(ctx, key, pl)
}

func applyDelta(cur uint64, delta int64) uint64 {
	if delta < 0 {
		dec := uint64(-delta)
		if dec > cur {
			return 0
		}
		return cur - dec
	}
	return cur + uint64(delta)
}

// applyQuoteDelta updates the quote aggregate with signed overflow detection.
// Positive deltas that would push the sum past `math.MaxUint64` return
// `ErrPriceLevelOverflow` so the caller can reject the underlying order.
func applyQuoteDelta(cur uint64, delta int64) (uint64, error) {
	if delta < 0 {
		dec := uint64(-delta)
		if dec > cur {
			return 0, nil
		}
		return cur - dec, nil
	}
	add := uint64(delta)
	if cur > maxUint64-add {
		return 0, types.ErrPriceLevelOverflow
	}
	return cur + add, nil
}

const maxUint64 = uint64(1<<64 - 1)

// checkedQuote returns base*price using big.Int and enforces the canonical
// `MaxOrderQuoteAmount` cap (≈2.8e14). Both factors are small enough to avoid
// intermediate overflow when the result is within the cap, but we still go
// through big.Int so the overflow path is guarded even if the cap changes.
func checkedQuote(base, price uint64) (uint64, error) {
	if base == 0 || price == 0 {
		return 0, nil
	}
	// Fast path: for values within uint64 we detect overflow by dividing.
	product := new(big.Int).Mul(
		new(big.Int).SetUint64(base),
		new(big.Int).SetUint64(price),
	)
	quoteCap := math.NewIntFromUint64(perptypes.MaxOrderQuoteAmount).BigInt()
	if product.Cmp(quoteCap) > 0 {
		return 0, types.ErrQuoteOverflow
	}
	return product.Uint64(), nil
}

// ComputeImpactPrice walks price levels on the requested side until it
// absorbs `MarketImpactNotional`, then returns the VWAP across that depth.
// Returns (0, false) when depth is insufficient or the market has not been
// initialised.
//
// Walk: bid side from highest price down, ask side from lowest up. The
// last partial level uses `ceil_div(needQuote, price)` so the notional is
// never under-filled. The final VWAP rounds UP for asks and DOWN for bids
// so that `max(0, idx - ask)` cannot round in the trader's favour.
func (k Keeper) ComputeImpactPrice(ctx context.Context, market uint32, isAsk bool) (uint32, bool, error) {
	notional, err := k.MarketImpactNotional(ctx, market)
	if err != nil {
		return 0, false, err
	}
	if notional == 0 {
		return 0, false, nil
	}

	rng := collections.NewPrefixedPairRange[uint32, uint32](market)
	iter, err := k.PriceLevels.Iterate(ctx, rng)
	if err != nil {
		return 0, false, err
	}
	defer iter.Close()

	type level struct {
		price uint32
		base  uint64
		quote uint64
	}
	var levels []level
	for ; iter.Valid(); iter.Next() {
		v, err := iter.Value()
		if err != nil {
			return 0, false, err
		}
		if isAsk {
			if v.AskBaseSum > 0 {
				levels = append(levels, level{v.Price, v.AskBaseSum, v.AskQuoteSum})
			}
		} else {
			if v.BidBaseSum > 0 {
				levels = append(levels, level{v.Price, v.BidBaseSum, v.BidQuoteSum})
			}
		}
	}
	if len(levels) == 0 {
		return 0, false, nil
	}
	if !isAsk {
		// Bid side: walk highest price first (reverse iterator order).
		for i, j := 0, len(levels)-1; i < j; i, j = i+1, j-1 {
			levels[i], levels[j] = levels[j], levels[i]
		}
	}

	var accBase, accQuote uint64
	for _, lv := range levels {
		// {Ask,Bid}QuoteSum stores raw `base * price` (no
		// quote_multiplier scaling, see adjustPriceLevel), so it is
		// directly comparable to `notional`.
		if accQuote+lv.quote >= notional {
			needQuote := notional - accQuote
			// Ceiling: floor would leave us short of `notional` on a
			// fractional level and (incorrectly) report insufficient
			// depth. Gated on `lv.base * price >= needQuote`, so it
			// never overshoots `lv.base`.
			needBase := (needQuote + uint64(lv.price) - 1) / uint64(lv.price)
			if needBase == 0 {
				needBase = 1
			}
			accBase += needBase
			accQuote += needBase * uint64(lv.price)
			break
		}
		accBase += lv.base
		accQuote += lv.quote
	}
	if accQuote < notional || accBase == 0 {
		return 0, false, nil
	}
	if isAsk {
		return uint32((accQuote + accBase - 1) / accBase), true, nil
	}
	return uint32(accQuote / accBase), true, nil
}

// MarketImpactNotional returns the per-market impact notional used by
// ComputeImpactPrice:
//
//	impactNotional = floor(perptypes.ImpactUSDCAmount * MarginTick
//	                       / MinInitialMarginFraction)
//
// `QuoteMultiplier` is intentionally NOT in the divisor: perpdex-l1
// treats it as 1 and `{Ask,Bid}QuoteSum` already stores raw `base*price`,
// so the two conventions cancel. Any future non-unit `QuoteMultiplier`
// must be introduced here AND in adjustPriceLevel together.
//
// `ImpactUSDCAmount * MarginTick` is computed in big.Int to stay safe
// against future ImpactUSDCAmount bumps. Returns 0 when
// `MinInitialMarginFraction == 0` (uninitialised market), which
// ComputeImpactPrice treats as insufficient depth.
func (k Keeper) MarketImpactNotional(ctx context.Context, market uint32) (uint64, error) {
	d, err := k.marketKeeper.GetMarketDetails(ctx, market)
	if err != nil {
		return 0, err
	}
	if d.MinInitialMarginFraction == 0 {
		return 0, nil
	}
	num := new(big.Int).Mul(
		new(big.Int).SetUint64(perptypes.ImpactUSDCAmount),
		new(big.Int).SetUint64(uint64(perptypes.MarginTick)),
	)
	out := new(big.Int).Quo(num, new(big.Int).SetUint64(uint64(d.MinInitialMarginFraction)))
	if !out.IsUint64() {
		// Refuse rather than silently truncate into the orderbook walker.
		return 0, types.ErrInvariantViolated.Wrapf(
			"impact_notional overflow: market=%d min_imf=%d",
			market, d.MinInitialMarginFraction,
		)
	}
	return out.Uint64(), nil
}

// BestBidAsk returns the best bid and best ask prices.
func (k Keeper) BestBidAsk(ctx context.Context, market uint32) (uint32, uint32, error) {
	bid, ok, err := k.PeekBestOpposite(ctx, market, true) // best bid is opposite of asks
	if err != nil {
		return 0, 0, err
	}
	var bidP uint32
	if ok {
		bidP = bid.Price
	}
	ask, ok, err := k.PeekBestOpposite(ctx, market, false) // best ask is opposite of bids
	if err != nil {
		return 0, 0, err
	}
	var askP uint32
	if ok {
		askP = ask.Price
	}
	return bidP, askP, nil
}

// indexClientOrder records the (market, account, client_order_index) -> order_index mapping.
//
// Internal helper. External callers go through OpenOrder / OpenTriggerOrder.
func (k Keeper) indexClientOrder(ctx context.Context, o types.Order) error {
	if o.ClientOrderIndex == 0 {
		return nil
	}
	return k.UserOrderIndex.Set(ctx, collections.Join3(o.MarketIndex, o.OwnerAccountIndex, o.ClientOrderIndex), o.OrderIndex)
}

// unindexClientOrder removes the (market, account, client_order_index) -> order mapping.
//
// Internal helper. External callers go through FillMakerOrder when a maker
// fully fills.
func (k Keeper) unindexClientOrder(ctx context.Context, o types.Order) error {
	if o.ClientOrderIndex == 0 {
		return nil
	}
	return k.UserOrderIndex.Remove(ctx, collections.Join3(o.MarketIndex, o.OwnerAccountIndex, o.ClientOrderIndex))
}

// unindexClientOrderIfMatches is a conditional unindex used by Cancel /
// Modify so an old order's cleanup cannot accidentally delete the mapping
// for a newer order that re-used the same client_order_index.
//
// Internal helper. External callers go through CancelOrder / EvictMakerOrder.
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

// HasOpenClientOrder returns (true, orderIndex) when the (market, account,
// clientID) tuple currently maps to an order whose status is not terminal.
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

// indexAccountOpenOrder marks `o` as a non-terminal order owned by
// `o.OwnerAccountIndex`. Independent of client_order_index so cancel-all
// can find every resting order.
//
// Internal helper. External callers go through OpenOrder / OpenTriggerOrder.
func (k Keeper) indexAccountOpenOrder(ctx context.Context, o types.Order) error {
	key := collections.Join(o.OwnerAccountIndex, o.OrderIndex)
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

// unindexAccountOpenOrder removes the (account, order_index) tuple. Safe
// to call on a tuple that was never indexed.
//
// Internal helper. External callers go through CancelOrder / FillMakerOrder /
// EvictMakerOrder.
func (k Keeper) unindexAccountOpenOrder(ctx context.Context, o types.Order) error {
	key := collections.Join(o.OwnerAccountIndex, o.OrderIndex)
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
// (+1 / -1). The counter never goes below zero — clamping protects
// against stale unindex calls from genesis or test fixtures that did
// not write the matching index entry.
func (k Keeper) bumpAccountOpenOrderCount(ctx context.Context, accIdx uint64, marketIdx uint32, delta int32) error {
	key := collections.Join(accIdx, marketIdx)
	cur, err := k.AccountOpenOrderCount.Get(ctx, key)
	if err != nil && !errors.Is(err, collections.ErrNotFound) {
		return err
	}
	next := int64(cur) + int64(delta)
	if next < 0 {
		next = 0
	}
	if next == 0 {
		// Avoid persisting trivial zero rows.
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
// for `account`. When `marketFilter != 0` only orders whose MarketIndex
// equals `marketFilter` are yielded; passing 0 yields all markets.
// Callers can return true from `cb` to stop early.
func (k Keeper) IterateAccountOpenOrders(
	ctx context.Context,
	account uint64,
	marketFilter uint32,
	cb func(types.Order) bool,
) error {
	rng := collections.NewPrefixedPairRange[uint64, uint64](account)
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
		o, err := k.GetOrder(ctx, key.K2())
		if err != nil {
			if errors.Is(err, types.ErrOrderNotFound) {
				continue
			}
			return err
		}
		if marketFilter != 0 && o.MarketIndex != marketFilter {
			continue
		}
		if cb(o) {
			return nil
		}
	}
	return nil
}

// addTrigger registers an order with a trigger price.
//
// Internal helper. External callers go through OpenTriggerOrder.
func (k Keeper) addTrigger(ctx context.Context, market uint32, triggerPrice uint32, orderIndex uint64) error {
	return k.TriggerIndex.Set(ctx, collections.Join3(market, triggerPrice, orderIndex))
}

// removeTrigger drops the trigger entry for an order.
//
// Internal helper. External callers go through ActivateTrigger / CancelOrder.
func (k Keeper) removeTrigger(ctx context.Context, market uint32, triggerPrice uint32, orderIndex uint64) error {
	return k.TriggerIndex.Remove(ctx, collections.Join3(market, triggerPrice, orderIndex))
}

// IterateTriggers yields every (market, triggerPrice, orderIndex) entry in
// the trigger index in ascending key order. The callback may return true to
// stop early.
func (k Keeper) IterateTriggers(ctx context.Context, cb func(market uint32, triggerPrice uint32, orderIndex uint64) bool) error {
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
		if cb(key.K1(), key.K2(), key.K3()) {
			return nil
		}
	}
	return nil
}

// IsExpired reports whether the order has passed its expiry.
func IsExpired(o types.Order, now int64) bool {
	if o.TimeInForce != perptypes.GTT {
		return false
	}
	return o.Expiry > 0 && now >= o.Expiry
}
