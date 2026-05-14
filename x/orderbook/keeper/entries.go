// entries.go is the orderbook keeper's "entry + price-level" layer.
// It is intentionally Order-agnostic — none of the functions in this
// file read or mutate Order records, indexes, or spot locks. The only
// state it owns is:
//
//   - OrderBookEntries[(market, side|sortKey)] -> OrderBookEntry
//   - OrderToSortKey[(market, order_index)] -> sortKey
//   - PriceLevels[(market, price)] -> PriceLevelAggregate
//
// Lifecycle operations (OpenOrder / CancelOrder / FillMakerOrder /
// EvictMakerOrder / ActivateTrigger) call into this layer alongside
// the orders.go layer; cross-layer composition lives in lifecycle.go.
//
// The split makes the dual-write contract between "entry residue" and
// "order residue" *explicit at the call site* rather than hidden
// behind a single helper: a lifecycle function reads as
// `shrinkEntryResidue(...); applyOrderResidue(...); update indexes`,
// so a reviewer can see both sides of the invariant in one place.
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

// insertEntry adds `o` to the sorted side store, records the
// (market, order_index) -> sortKey reverse mapping, and bumps the
// price-level aggregate by (base, quote, +1 count).
//
// Internal helper. External callers go through Keeper.OpenOrder
// in lifecycle.go.
func (k Keeper) insertEntry(ctx context.Context, market uint32, isAsk bool, o types.OrderBookEntry) error {
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

// removeEntry drops the entry, the reverse-mapping, and the price-level
// contribution. Returns nil if the entry is already absent (idempotent
// for cancel paths that may run on top of a partially-cleaned state).
//
// Internal helper. External callers go through Keeper.CancelOrder /
// Keeper.EvictMakerOrder in lifecycle.go.
func (k Keeper) removeEntry(ctx context.Context, market uint32, isAsk bool, orderIndex uint64) error {
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

// shrinkEntryResidue subtracts `delta` from the resting entry's
// remaining_base, removes the entry when residue reaches zero, and
// reflects the change in the price-level aggregate. Returns:
//
//   - `entryRemoved`: true when the entry was drained and deleted.
//     Lifecycle callers cross-check this against the Order-side
//     `applyOrderResidue` result to assert the entry/order residue
//     invariant.
//   - `filledQuote`:  the CheckedQuote(delta, price) — surfaced so
//     callers that want to emit / log the consumed notional do not
//     have to recompute it.
//
// `delta` is clamped to the entry's current residue; passing a larger
// value just drains the entry rather than underflowing.
//
// Internal helper. External callers go through Keeper.FillMakerOrder
// in lifecycle.go.
func (k Keeper) shrinkEntryResidue(
	ctx context.Context, market uint32, isAsk bool, orderIndex uint64, delta uint64,
) (entryRemoved bool, filledQuote uint64, err error) {
	sk, err := k.OrderToSortKey.Get(ctx, collections.Join(market, orderIndex))
	if err != nil {
		return false, 0, err
	}
	tripKey := collections.Join(market, sk)
	entry, err := k.OrderBookEntries.Get(ctx, tripKey)
	if err != nil {
		return false, 0, err
	}
	if delta > entry.RemainingBaseAmount {
		delta = entry.RemainingBaseAmount
	}
	entry.RemainingBaseAmount -= delta
	// Use CheckedQuote so a corrupt entry or a future cap change cannot
	// silently overflow the price-level aggregate.
	filledQuote, err = CheckedQuote(delta, uint64(entry.Price))
	if err != nil {
		return false, 0, err
	}
	if err := k.adjustPriceLevel(ctx, market, isAsk, entry.Price, delta, filledQuote, -1, 0); err != nil {
		return false, 0, err
	}
	if entry.RemainingBaseAmount == 0 {
		if err := k.OrderBookEntries.Remove(ctx, tripKey); err != nil {
			return false, 0, err
		}
		if err := k.OrderToSortKey.Remove(ctx, collections.Join(market, orderIndex)); err != nil {
			return false, 0, err
		}
		// The base and quote contributions were already subtracted
		// above; only the entry-count delta is left to apply.
		if err := k.adjustPriceLevel(ctx, market, isAsk, entry.Price, 0, 0, 0, -1); err != nil {
			return false, 0, err
		}
		return true, filledQuote, nil
	}
	if err := k.OrderBookEntries.Set(ctx, tripKey, entry); err != nil {
		return false, 0, err
	}
	return false, filledQuote, nil
}

// PeekBest returns the head-of-book entry on `side` (isAsk=true → best
// ask, lowest price first; isAsk=false → best bid, highest price first).
// Returns (entry, true, nil) if an order exists on the requested side.
//
// The iterator is scoped to the (market, side) byte range so a peek
// pays only the depth of the requested side — bid lookups never walk
// the ask half of the book and vice versa.
func (k Keeper) PeekBest(ctx context.Context, market uint32, isAsk bool) (types.OrderBookEntry, bool, error) {
	rng := sidePrefixedRange(market, isAsk)
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

// WouldCross reports whether placing an order at `price` on `isAsk` side
// would match against the best opposite-side order.
func (k Keeper) WouldCross(ctx context.Context, market uint32, isAsk bool, price uint32) (bool, error) {
	best, ok, err := k.PeekBest(ctx, market, !isAsk)
	if err != nil || !ok {
		return false, err
	}
	if isAsk {
		return price <= best.Price, nil
	}
	return price >= best.Price, nil
}

// BestBidAsk returns the best bid and best ask prices for `market`. A
// zero on either side indicates that side of the book is empty.
func (k Keeper) BestBidAsk(ctx context.Context, market uint32) (uint32, uint32, error) {
	bid, ok, err := k.PeekBest(ctx, market, false)
	if err != nil {
		return 0, 0, err
	}
	var bidP uint32
	if ok {
		bidP = bid.Price
	}
	ask, ok, err := k.PeekBest(ctx, market, true)
	if err != nil {
		return 0, 0, err
	}
	var askP uint32
	if ok {
		askP = ask.Price
	}
	return bidP, askP, nil
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
		nextCount, err := ApplyCountDelta(pl.AskCount, countDelta)
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
		nextCount, err := ApplyCountDelta(pl.BidCount, countDelta)
		if err != nil {
			return err
		}
		pl.BidCount = nextCount
	}
	// Cross-field invariant: the price level was built from
	// (base, quote, count) contributions added together, so all six
	// aggregates must drop to zero in lock-step. Cleaning up only on
	// "base + count == 0" left a window where a future bug could leave
	// QuoteSum > 0 sitting on a zero-base row; the explicit conjunction
	// catches that drift instead of persisting a half-empty level.
	if pl.AskBaseSum == 0 && pl.BidBaseSum == 0 &&
		pl.AskQuoteSum == 0 && pl.BidQuoteSum == 0 &&
		pl.AskCount == 0 && pl.BidCount == 0 {
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

// ApplyCountDelta enforces the same strict invariant on the per-side
// entry-count aggregate. The price level is created together with its
// first entry and torn down together with the last one, so any negative
// adjustment that would underflow is a bug. The upper bound is the
// proto field's full uint32 range, matching bumpAccountOpenOrderCount.
func ApplyCountDelta(cur uint32, delta int32) (uint32, error) {
	next := int64(cur) + int64(delta)
	if next < 0 {
		return 0, types.ErrInvariantViolated.Wrapf(
			"price-level count under-subtract: cur=%d delta=%d", cur, delta,
		)
	}
	if next > stdmath.MaxUint32 {
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
		return 0, types.ErrInvariantViolated.Wrapf(
			"impact_notional overflow: market=%d min_imf=%d quote_multiplier=%d",
			market, d.MinInitialMarginFraction, d.QuoteMultiplier,
		)
	}
	return out.Uint64(), nil
}
