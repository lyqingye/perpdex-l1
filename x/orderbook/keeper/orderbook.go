package keeper

import (
	"context"
	"errors"
	stdmath "math"
	"math/big"

	"cosmossdk.io/collections"
	sdkmath "cosmossdk.io/math"

	perptypes "github.com/perpdex/perpdex-l1/types"
	"github.com/perpdex/perpdex-l1/x/orderbook/types"
)

// AllocateOrderIndex returns the next order_index. The 1-based invariant is
// established up-front in InitGenesis (NextOrderIndex is forced to >= 1
// during boot), so this is a straight Sequence.Next with no skip-zero
// branch.
func (k Keeper) AllocateOrderIndex(ctx context.Context) (uint64, error) {
	return k.NextOrderIndex.Next(ctx)
}

// insertOrderbookEntry adds an order to the orderbook (sorted side store) and
// updates the price-level aggregate. The per-entry quote notional is capped
// by `perptypes.MaxOrderQuoteAmount` (~2.8e14) and the price-level aggregate
// is guarded against uint64 overflow in `adjustPriceLevel`.
//
// Internal helper. External callers go through OpenOrder.
func (k Keeper) insertOrderbookEntry(ctx context.Context, market uint32, isAsk bool, o types.OrderBookEntry) error {
	quote, err := CheckedQuote(o.RemainingBaseAmount, uint64(o.Price))
	if err != nil {
		return err
	}
	sk := types.SortableKey(o.Price, o.Nonce, isAsk)
	if err := k.OrderBookEntries.Set(ctx, collections.Join(market, sk), o); err != nil {
		return err
	}
	if err := k.OrderToSortKey.Set(ctx, collections.Join(market, o.OrderIndex), sk); err != nil {
		return err
	}
	return k.adjustPriceLevel(ctx, market, isAsk, o.Price, o.RemainingBaseAmount, quote, +1, +1)
}

// removeOrderbookEntry deletes the entry and decrements the price-level.
//
// Internal helper. External callers go through CancelOrder / EvictMakerOrder.
func (k Keeper) removeOrderbookEntry(ctx context.Context, market uint32, isAsk bool, orderIndex uint64) error {
	sk, err := k.OrderToSortKey.Get(ctx, collections.Join(market, orderIndex))
	if err != nil {
		if errors.Is(err, collections.ErrNotFound) {
			return nil
		}
		return err
	}
	entry, err := k.OrderBookEntries.Get(ctx, collections.Join(market, sk))
	if err != nil {
		if errors.Is(err, collections.ErrNotFound) {
			return nil
		}
		return err
	}
	if err := k.OrderBookEntries.Remove(ctx, collections.Join(market, sk)); err != nil {
		return err
	}
	if err := k.OrderToSortKey.Remove(ctx, collections.Join(market, orderIndex)); err != nil {
		return err
	}
	// Remove the notional contribution computed the same way the insert
	// added it. Entries that survive have already passed the quote cap
	// on insert so the multiply never overflows here.
	quote, err := CheckedQuote(entry.RemainingBaseAmount, uint64(entry.Price))
	if err != nil {
		return err
	}
	return k.adjustPriceLevel(ctx, market, isAsk, entry.Price, entry.RemainingBaseAmount, quote, -1, -1)
}

// partialFill subtracts filledBase from the remaining_base_amount of the entry
// and updates the price level. If remaining drops to 0 the entry is removed.
//
// Internal helper. External callers go through FillMakerOrder.
func (k Keeper) partialFill(ctx context.Context, market uint32, isAsk bool, orderIndex uint64, filledBase uint64) error {
	sk, err := k.OrderToSortKey.Get(ctx, collections.Join(market, orderIndex))
	if err != nil {
		return err
	}
	tripKey := collections.Join(market, sk)
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
	filledQuote, err := CheckedQuote(filledBase, uint64(entry.Price))
	if err != nil {
		return err
	}
	if err := k.adjustPriceLevel(ctx, market, isAsk, entry.Price, filledBase, filledQuote, -1, 0); err != nil {
		return err
	}
	if entry.RemainingBaseAmount == 0 {
		if err := k.OrderBookEntries.Remove(ctx, tripKey); err != nil {
			return err
		}
		if err := k.OrderToSortKey.Remove(ctx, collections.Join(market, orderIndex)); err != nil {
			return err
		}
		// The base and quote contributions were already subtracted
		// above; only the entry-count delta is left to apply.
		return k.adjustPriceLevel(ctx, market, isAsk, entry.Price, 0, 0, 0, -1)
	}
	return k.OrderBookEntries.Set(ctx, tripKey, entry)
}

// PeekBestOpposite returns the head-of-book entry on the opposite side of
// `isAsk`. Returns (entry, true, nil) if an order exists.
//
// The iterator is scoped to the (market, opposite-side) byte range so it
// does not walk across the other side of the book — best bid lookups
// won't pay an O(asks) traversal and vice versa.
func (k Keeper) PeekBestOpposite(ctx context.Context, market uint32, isAsk bool) (types.OrderBookEntry, bool, error) {
	rng := sidePrefixedRange(market, !isAsk)
	iter, err := k.OrderBookEntries.Iterate(ctx, rng)
	if err != nil {
		return types.OrderBookEntry{}, false, err
	}
	defer iter.Close()
	if !iter.Valid() {
		return types.OrderBookEntry{}, false, nil
	}
	v, err := iter.Value()
	if err != nil {
		return types.OrderBookEntry{}, false, err
	}
	return v, true, nil
}

// sidePrefixedRange returns a Range covering exactly the entries on the
// requested side of `market`. We rely on the fact that SortableKey
// prefixes every key with the side discriminator byte (see
// types.SortableKey): the range is `[market || side_byte || 0...0,
// market || (side_byte+1) || 0...0)`.
func sidePrefixedRange(market uint32, isAsk bool) *collections.Range[collections.Pair[uint32, []byte]] {
	side := types.SideByte(isAsk)
	start := make([]byte, types.SortableKeyLen)
	start[0] = side
	end := make([]byte, types.SortableKeyLen)
	end[0] = side + 1
	rng := new(collections.Range[collections.Pair[uint32, []byte]]).
		StartInclusive(collections.Join(market, start)).
		EndExclusive(collections.Join(market, end))
	return rng
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
// (market, price) for the given side. All deltas are split into
// magnitude (`baseMag` / `quoteMag`, both uint64) and direction (`sign`,
// either +1 or -1) so we never round-trip through `int64` — a value
// approaching `MaxOrderQuoteAmount` never gets silently truncated when
// cast.
//
// `countDelta` is the +/-1 entry-count delta. The quote aggregate is
// guarded against uint64 overflow (returning ErrPriceLevelOverflow) on
// positive deltas, and against under-subtraction (returning
// ErrInvariantViolated) on negative deltas — the price level was built
// from the same contributions the runtime removes, so a "remove > add"
// situation is a state-machine bug, never a normal flow.
func (k Keeper) adjustPriceLevel(
	ctx context.Context,
	market uint32,
	isAsk bool,
	price uint32,
	baseMag, quoteMag uint64,
	sign int8,
	countDelta int32,
) error {
	if sign != +1 && sign != -1 && sign != 0 {
		return types.ErrInvariantViolated.Wrapf("adjustPriceLevel: bad sign=%d", sign)
	}
	key := collections.Join(market, price)
	pl, err := k.PriceLevels.Get(ctx, key)
	if err != nil && !errors.Is(err, collections.ErrNotFound) {
		return err
	}
	if errors.Is(err, collections.ErrNotFound) {
		pl = types.PriceLevelAggregate{MarketIndex: market, Price: price}
	}
	if isAsk {
		next, err := ApplyMagDelta(pl.AskBaseSum, baseMag, sign)
		if err != nil {
			return err
		}
		pl.AskBaseSum = next
		nextQuote, err := ApplyMagDelta(pl.AskQuoteSum, quoteMag, sign)
		if err != nil {
			return err
		}
		pl.AskQuoteSum = nextQuote
		nextCount, err := applyCountDelta(pl.AskCount, countDelta)
		if err != nil {
			return err
		}
		pl.AskCount = nextCount
	} else {
		next, err := ApplyMagDelta(pl.BidBaseSum, baseMag, sign)
		if err != nil {
			return err
		}
		pl.BidBaseSum = next
		nextQuote, err := ApplyMagDelta(pl.BidQuoteSum, quoteMag, sign)
		if err != nil {
			return err
		}
		pl.BidQuoteSum = nextQuote
		nextCount, err := applyCountDelta(pl.BidCount, countDelta)
		if err != nil {
			return err
		}
		pl.BidCount = nextCount
	}
	if pl.AskBaseSum == 0 && pl.BidBaseSum == 0 && pl.AskCount == 0 && pl.BidCount == 0 {
		return k.PriceLevels.Remove(ctx, key)
	}
	return k.PriceLevels.Set(ctx, key, pl)
}

// ApplyMagDelta is the strict signed-delta primitive used by
// adjustPriceLevel. `sign == 0` is a no-op; `sign == +1` adds `mag` and
// errors on uint64 overflow; `sign == -1` subtracts `mag` and errors on
// under-subtraction — the aggregate is built from the same entries the
// runtime removes, so an under-subtract is a state-machine bug.
func ApplyMagDelta(cur uint64, mag uint64, sign int8) (uint64, error) {
	switch sign {
	case 0:
		return cur, nil
	case +1:
		if cur > stdmath.MaxUint64-mag {
			return 0, types.ErrPriceLevelOverflow
		}
		return cur + mag, nil
	case -1:
		if mag > cur {
			return 0, types.ErrInvariantViolated.Wrapf(
				"price-level aggregate under-subtract: cur=%d delta=-%d", cur, mag,
			)
		}
		return cur - mag, nil
	default:
		return 0, types.ErrInvariantViolated.Wrapf("ApplyMagDelta: bad sign=%d", sign)
	}
}

// applyCountDelta enforces the same strict invariant on the per-side
// entry-count aggregate. The price level is created together with its
// first entry and torn down together with the last one, so any negative
// adjustment that would underflow is a bug.
func applyCountDelta(cur uint32, delta int32) (uint32, error) {
	next := int64(cur) + int64(delta)
	if next < 0 {
		return 0, types.ErrInvariantViolated.Wrapf(
			"price-level count under-subtract: cur=%d delta=%d", cur, delta,
		)
	}
	if next > stdmath.MaxInt32 {
		return 0, types.ErrPriceLevelOverflow.Wrapf(
			"price-level count overflow: cur=%d delta=%d", cur, delta,
		)
	}
	return uint32(next), nil
}

// CheckedQuote returns base*price and enforces two guards:
//
//   - `MaxOrderQuoteAmount` cap (~2.8e14): per-order notional ceiling so
//     a single order cannot dominate the orderbook bookkeeping.
//   - `math.MaxInt64` cap: even though today's `MaxOrderQuoteAmount` is
//     well below `MaxInt64`, the price-level math and the `int64`
//     conversions performed by the caller (and by future code paths)
//     rely on the quote fitting in a signed 64-bit integer. The
//     assertion is cheap; it makes a future bump of
//     `MaxOrderQuoteAmount` impossible to do silently.
//
// `base == 0 || price == 0 → (0, nil)` keeps the boundary smooth for
// callers that build entries from optional-zero fields.
func CheckedQuote(base, price uint64) (uint64, error) {
	if base == 0 || price == 0 {
		return 0, nil
	}
	product := new(big.Int).Mul(
		new(big.Int).SetUint64(base),
		new(big.Int).SetUint64(price),
	)
	quoteCap := sdkmath.NewIntFromUint64(perptypes.MaxOrderQuoteAmount).BigInt()
	if product.Cmp(quoteCap) > 0 {
		return 0, types.ErrQuoteOverflow
	}
	// Belt and suspenders: if MaxOrderQuoteAmount is ever raised above
	// MaxInt64, fail loud rather than overflow downstream int64 casts.
	if !product.IsInt64() {
		return 0, types.ErrQuoteOverflow.Wrapf(
			"product %s exceeds math.MaxInt64", product.String(),
		)
	}
	return product.Uint64(), nil
}

// ComputeImpactPrice walks price levels on the requested side until it
// absorbs `MarketImpactNotional`, then returns the VWAP across that depth.
// Returns 0 when depth is insufficient or the market has not been
// initialised; callers treat a zero VWAP as "side unavailable".
//
// Walk: bid side from highest price down (iterator runs in descending
// order), ask side from lowest up. The last partial level uses
// `ceil_div(needQuote, price)` so the notional is never under-filled.
// The final VWAP rounds UP for asks and DOWN for bids so that
// `max(0, idx - ask)` cannot round in the trader's favour.
func (k Keeper) ComputeImpactPrice(ctx context.Context, market uint32, isAsk bool) (uint32, error) {
	notional, err := k.MarketImpactNotional(ctx, market)
	if err != nil {
		return 0, err
	}
	if notional == 0 {
		return 0, nil
	}

	rng := collections.NewPrefixedPairRange[uint32, uint32](market)
	if !isAsk {
		// Bid VWAP must walk the highest price first.
		rng = rng.Descending()
	}
	iter, err := k.PriceLevels.Iterate(ctx, rng)
	if err != nil {
		return 0, err
	}
	defer iter.Close()

	var accBase, accQuote uint64
	for ; iter.Valid(); iter.Next() {
		v, err := iter.Value()
		if err != nil {
			return 0, err
		}
		var lvBase, lvQuote uint64
		if isAsk {
			lvBase, lvQuote = v.AskBaseSum, v.AskQuoteSum
		} else {
			lvBase, lvQuote = v.BidBaseSum, v.BidQuoteSum
		}
		if lvBase == 0 {
			continue
		}
		// `accQuote` and `notional` must share the same quote-scale
		// convention. Today `{Ask,Bid}QuoteSum` is raw `base*price`
		// and `notional` is computed with QuoteMultiplier=1 in
		// MarketImpactNotional, so the comparison is direct.
		// See the INVARIANT note on MarketImpactNotional.
		if accQuote+lvQuote >= notional {
			needQuote := notional - accQuote
			// Ceiling: floor would leave us short of `notional` on a
			// fractional level and (incorrectly) report insufficient
			// depth. Gated on `lvBase * price >= needQuote`, so it
			// never overshoots `lvBase`.
			price := uint64(v.Price)
			needBase := (needQuote + price - 1) / price
			if needBase == 0 {
				needBase = 1
			}
			accBase += needBase
			accQuote += needBase * price
			break
		}
		accBase += lvBase
		accQuote += lvQuote
	}
	if accQuote < notional || accBase == 0 {
		return 0, nil
	}
	if isAsk {
		return uint32((accQuote + accBase - 1) / accBase), nil
	}
	return uint32(accQuote / accBase), nil
}

// MarketImpactNotional returns the per-market impact notional used by
// ComputeImpactPrice:
//
//	impactNotional = floor(ImpactUSDCAmount * MarginTick
//	                       / (MinInitialMarginFraction * QuoteMultiplier))
//
// `QuoteMultiplier` is a per-market quote-scale tick on MarketDetails.
// perpdex-l1 currently leaves it unset (resetRuntimeDetails zeros it on
// CreateMarket and no module writes it back), and `adjustPriceLevel`
// stores `{Ask,Bid}QuoteSum` as raw `base*price` without scaling by it.
// The divisor is kept in the formula to preserve the canonical shape so
// activating QuoteMultiplier later is a localised change; a zero value
// falls back to 1 so today's behaviour is bit-for-bit identical to the
// pre-formula version.
//
// INVARIANT: if `QuoteMultiplier` is ever made non-trivial here,
// `adjustPriceLevel` must start multiplying the price-level quote
// aggregate by the same factor — otherwise the two sides of the
// `accQuote >= notional` comparison in ComputeImpactPrice end up in
// different units.
//
// Returns 0 when `MinInitialMarginFraction == 0` (uninitialised market),
// which ComputeImpactPrice treats as insufficient depth.
func (k Keeper) MarketImpactNotional(ctx context.Context, market uint32) (uint64, error) {
	d, err := k.marketKeeper.GetMarketDetails(ctx, market)
	if err != nil {
		return 0, err
	}
	if d.MinInitialMarginFraction == 0 {
		return 0, nil
	}
	qm := uint64(d.QuoteMultiplier)
	if qm == 0 {
		qm = 1
	}
	num := new(big.Int).Mul(
		new(big.Int).SetUint64(perptypes.ImpactUSDCAmount),
		new(big.Int).SetUint64(uint64(perptypes.MarginTick)),
	)
	den := new(big.Int).Mul(
		new(big.Int).SetUint64(uint64(d.MinInitialMarginFraction)),
		new(big.Int).SetUint64(qm),
	)
	out := new(big.Int).Quo(num, den)
	if !out.IsUint64() {
		// Refuse rather than silently truncate into the orderbook walker.
		return 0, types.ErrInvariantViolated.Wrapf(
			"impact_notional overflow: market=%d min_imf=%d quote_multiplier=%d",
			market, d.MinInitialMarginFraction, d.QuoteMultiplier,
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
// Internal helper. External callers go through OpenOrder / OpenTriggerOrder.
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
// Internal helper. External callers go through CancelOrder / FillMakerOrder /
// EvictMakerOrder.
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
// Callback contract: returning `true` STOPS iteration; returning
// `false` continues. The stop-on-true convention matches IterateTriggers
// and the upstream Cosmos collections iterator style.
func (k Keeper) IterateAccountOpenOrders(
	ctx context.Context,
	account uint64,
	marketFilter uint32,
	cb func(types.Order) bool,
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
// the trigger index in ascending key order.
//
// Callback contract: returning `true` STOPS iteration; returning
// `false` continues.
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

// addExpiryIndex registers `o` in the GTT expiry index so EndBlocker
// can walk only the orders due to expire by `now`. Non-GTT orders and
// GTT orders with `Expiry == 0` are not indexed (they never expire).
//
// Internal helper. External callers go through OpenOrder /
// OpenTriggerOrder (any time an order with GTT semantics is being
// persisted onto / kept onto the book).
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
//
// Internal helper. External callers go through CancelOrder /
// FillMakerOrder / EvictMakerOrder / OpenOrder (any time an order
// leaves the book or moves to a terminal status).
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
// block does O(due_orders) work instead of O(history).
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
