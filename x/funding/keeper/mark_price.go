package keeper

import (
	"context"

	"cosmossdk.io/math"

	perptypes "github.com/perpdex/perpdex-l1/types"
)

// This file owns the per-block mark price refresh. It rewrites
// `MarketDetails.MarkPrice` every block as a median3 of the impact
// price, an EMA-smoothed index+premium curve, and the oracle mark
// price, with degenerate inputs handled via mean / passthrough
// fallbacks.

// refreshMarkPrice rewrites `MarketDetails.MarkPrice` every block as
//
//	premium_raw  = clamp(impact_price - index, ±index/MarkPremiumClampDivisor)
//	premium_ema  = ema_step(prev_ema, premium_raw, dt, MarkPremiumEmaTauMs)
//	price_1      = clampUint32(index + premium_ema)
//	price_2      = oracle weighted-median markPrice
//	markPrice         = median3(impact_price, price_1, price_2)
//
// The EMA state is persisted on `MarketDetails.MarkPremiumEma` so it
// survives across blocks. `dt` is derived from
// `now - LastMarkPriceRefreshTimestamp`; on first call (timestamp 0) or after a
// gap >= TAU the EMA is reset to `premium_raw` so a long funding stall
// cannot leave stale momentum.
//
// Fallbacks: oracle failure returns early (leaves MarkPrice untouched so
// the staleness gate eventually fires); zero impact_price / price_1 /
// price_2 degrade to the mean of the remaining non-zero inputs; all
// three zero keeps the previous markPrice.
//
// On success `LastMarkPriceRefreshTimestamp` is bumped to `now` (distinct from
// `LastPremiumSampleTimestamp`, which throttles `processMarketSample`).
func (k Keeper) refreshMarkPrice(ctx context.Context, marketIdx uint32, now int64) {
	d, err := k.marketKeeper.GetMarketDetails(ctx, marketIdx)
	if err != nil {
		return
	}
	// Oracle failure leaves MarkPrice/LastMarkPriceRefreshTimestamp untouched
	// so the downstream staleness gate eventually trips. Index/markPrice
	// from the oracle may individually be 0 (degenerate aggregation);
	// the median switch below handles that.
	px, err := k.oracleKeeper.GetPrice(ctx, marketIdx)
	if err != nil {
		return
	}
	// Preserve the previous d.IndexPrice on a px.IndexPrice == 0
	// reading so the markPrice path retains a meaningful clamp bound.
	// Note: this only delivers across the ~11/12 blocks per minute
	// where processMarketSample is throttled out by
	// premiumSampleIntervalMs. When both call sites run in the same
	// block, processMarketSample writes d.IndexPrice = px.IndexPrice
	// unconditionally first, so a zero idx is already persisted before
	// we get here. The asymmetry is acceptable because
	// processMarketSample explicitly short-circuits on d.IndexPrice == 0
	// (it would otherwise divide by zero), while refreshMarkPrice can
	// still emit a useful markPrice via the median fallback even with a
	// stale d.IndexPrice.
	if px.IndexPrice != 0 {
		d.IndexPrice = px.IndexPrice
	}

	// premium_raw = clamp(impact - index, ±index/divisor). When impact
	// or index is 0 we cannot compute a meaningful premium, so raw is
	// forced to 0; emaStep then decays the existing EMA toward 0
	// (rather than freezing it). The decay rate is bounded by the
	// usual `dt / TAU` step, so a transient one-sided drain causes at
	// most ~`block_dt / TAU` proportional pull-back per block. This
	// matches the spec's "neutral premium when signal is missing"
	// semantic — freezing the last good EMA would let a stale spike
	// dominate markPrice price indefinitely on a halted book.
	idx := int64(d.IndexPrice)
	var rawPremium int64
	if d.ImpactPrice != 0 && idx > 0 {
		bound := idx / perptypes.MarkPremiumClampDivisor
		rawPremium = clampInt64(int64(d.ImpactPrice)-idx, -bound, bound)
	}

	dt := now - d.LastMarkPriceRefreshTimestamp
	ema := emaStep(d.MarkPremiumEma, rawPremium, dt, perptypes.MarkPremiumEmaTauMs, d.LastMarkPriceRefreshTimestamp == 0)
	// Re-clamp the persisted EMA against the CURRENT clamp band. Without
	// this an idx that just collapsed by >MarkPremiumClampDivisor× would
	// leave the stored momentum outside the new ±idx/200 band, and
	// `clampUint32(idx + ema)` could underflow to 0 — silently degrading
	// the median to mean(impact, oracle_mark) for many blocks because
	// emaStep can stall at zero-step (|raw-prev|*dt/tau < 1) when raw is
	// pinned near 0. We skip this when idx == 0; in that case rawPremium
	// is already 0 and emaStep decays prev toward 0 proportionally.
	if idx > 0 {
		b := idx / perptypes.MarkPremiumClampDivisor
		ema = clampInt64(ema, -b, b)
	}

	price1 := clampUint32(idx + ema)
	price2 := px.MarkPrice

	var markPrice uint32
	switch {
	case d.ImpactPrice != 0 && price1 != 0 && price2 != 0:
		markPrice = median3Uint32(d.ImpactPrice, price1, price2)
	case d.ImpactPrice != 0 && price1 != 0:
		markPrice = uint32((uint64(d.ImpactPrice) + uint64(price1)) / 2)
	case d.ImpactPrice != 0 && price2 != 0:
		markPrice = uint32((uint64(d.ImpactPrice) + uint64(price2)) / 2)
	case price1 != 0 && price2 != 0:
		markPrice = uint32((uint64(price1) + uint64(price2)) / 2)
	case d.ImpactPrice != 0:
		markPrice = d.ImpactPrice
	case price1 != 0:
		markPrice = price1
	case price2 != 0:
		markPrice = price2
	default:
		// All three inputs zero: keep the previous markPrice.
		return
	}
	d.MarkPrice = markPrice
	d.MarkPremiumEma = ema
	d.LastMarkPriceRefreshTimestamp = now
	k.mustSetMarketDetails(ctx, d)
}

// emaStep advances a discrete EMA with time constant `tau`:
//
//	ema_new = ema_old + (raw - ema_old) * dt / tau
//
// `reset` (true on the very first refresh) or `dt >= tau` (long outage)
// reseeds the EMA to `raw` so stale momentum is dropped. Negative `dt`
// (clock regression) is treated as a no-op step.
func emaStep(prev, raw, dt, tau int64, reset bool) int64 {
	if tau <= 0 || reset || dt >= tau {
		return raw
	}
	if dt <= 0 {
		return prev
	}
	// math.Int wraps the multiplication so `(raw - prev) * dt` cannot
	// overflow int64; the subtraction itself is bounded by
	// `±2 * idx / MarkPremiumClampDivisor` (≤ ~4.3e7 for a uint32
	// price) and safe in plain int64. Quo truncates toward zero, so
	// |delta| <= |raw - prev| in both directions; once
	// `|raw - prev| * dt / tau < 1` the step rounds to 0 and EMA
	// stalls at price-tick precision — acceptable for uint32 marks.
	delta := math.NewInt(raw - prev).
		Mul(math.NewInt(dt)).
		Quo(math.NewInt(tau))
	if delta.IsZero() {
		return prev
	}
	return prev + delta.Int64()
}
