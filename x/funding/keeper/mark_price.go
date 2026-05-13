package keeper

import (
	"context"

	"cosmossdk.io/math"

	perptypes "github.com/perpdex/perpdex-l1/types"
)

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
func (k Keeper) refreshMarkPrice(ctx context.Context, marketIdx uint32, now int64) {
	d, err := k.marketKeeper.GetMarketDetails(ctx, marketIdx)
	if err != nil {
		return
	}
	// Oracle failure leaves MarkPrice/timestamp untouched so the
	// staleness gate eventually trips. Individual zero fields from a
	// degenerate oracle aggregation are tolerated by the median switch
	// below.
	px, err := k.oracleKeeper.GetPrice(ctx, marketIdx)
	if err != nil {
		return
	}
	// Keep the cached IndexPrice on a zero oracle reading so the clamp
	// bound below stays meaningful. processMarketSample overwrites
	// unconditionally but short-circuits on idx == 0; refreshMarkPrice
	// can still emit a useful mark via the median fallback.
	if px.IndexPrice != 0 {
		d.IndexPrice = px.IndexPrice
	}

	// premium_raw = clamp(impact - index, ±index/divisor). When impact
	// or index is 0, raw stays 0 so emaStep decays prev toward 0;
	// freezing instead would let a stale spike dominate markPrice
	// indefinitely on a halted book.
	idx := int64(d.IndexPrice)
	var rawPremium int64
	if d.ImpactPrice != 0 && idx > 0 {
		bound := idx / perptypes.MarkPremiumClampDivisor
		rawPremium = clampInt64(int64(d.ImpactPrice)-idx, -bound, bound)
	}

	dt := now - d.LastMarkPriceRefreshTimestamp
	ema := emaStep(d.MarkPremiumEma, rawPremium, dt, perptypes.MarkPremiumEmaTauMs, d.LastMarkPriceRefreshTimestamp == 0)
	// Re-clamp the persisted EMA against the CURRENT band: if idx just
	// collapsed by >divisor×, stale ema could push `idx+ema` to underflow
	// and emaStep stalls when `|raw-prev|*dt/tau < 1`, so it cannot
	// self-heal on its own. Skipped when idx == 0; raw is already 0 and
	// emaStep decays prev.
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
	// math.Int avoids int64 overflow on `(raw-prev) * dt`. Quo truncates,
	// so |delta| <= |raw-prev|; once `|raw-prev|*dt/tau < 1` the step
	// rounds to 0 and the EMA stalls at tick precision.
	delta := math.NewInt(raw - prev).
		Mul(math.NewInt(dt)).
		Quo(math.NewInt(tau))
	if delta.IsZero() {
		return prev
	}
	return prev + delta.Int64()
}
