package keeper

import (
	"context"
	"fmt"
	"sort"

	"cosmossdk.io/math"

	sdk "github.com/cosmos/cosmos-sdk/types"

	perptypes "github.com/perpdex/perpdex-l1/types"
	"github.com/perpdex/perpdex-l1/x/funding/types"
	markettypes "github.com/perpdex/perpdex-l1/x/market/types"
)

// premiumSampleIntervalMs is the per-market spacing between two consecutive
// premium samples. We sample once a minute (60 samples per hour) on a
// per-market basis using `MarketDetails.LastPremiumSampleTimestamp`.
const premiumSampleIntervalMs = perptypes.MinuteInMs

// BeginBlocker runs the per-market funding pipeline:
//
//  1. processMarketSample (1-minute throttle): refresh impact prices and
//     push a premium sample
//     premium_t = (max(0, IB-idx) - max(0, idx-IA)) * FundingRateTick / idx.
//  2. refreshMarkPrice (every block): write the authoritative
//     `MarketDetails.MarkPrice` consumed by x/risk, x/trade and x/matching.
//  3. SettleAllMarkets (every `FundingPeriodMs`, default 1 hour): close
//     the round and bump `FundingRatePrefixSum`.
//
// Per-market business errors (stale oracle, one-sided depth, etc.) are
// silently swallowed so one bad market cannot abort the whole block.
// `SetMarketDetails` failures panic — the runtime store must never fail.
func (k Keeper) BeginBlocker(ctx context.Context) error {
	sdkCtx := sdk.UnwrapSDKContext(ctx)
	now := sdkCtx.BlockTime().UnixMilli()
	params, err := k.Params.Get(ctx)
	if err != nil {
		return err
	}
	meta, err := k.Metadata.Get(ctx)
	if err != nil {
		return err
	}
	if err := k.marketKeeper.IterateMarkets(ctx, func(m markettypes.Market) bool {
		if m.MarketType != perptypes.MarketTypePerps || m.Status != perptypes.MarketStatusActive {
			return false
		}
		// Per-market sampling errors (stale oracle / one-sided depth) are
		// expected and swallowed. The next block will retry automatically.
		k.processMarketSample(ctx, m.MarketIndex, now, params)
		// Refresh the authoritative markPrice every block (cheap: it just
		// medians three uint32 values plus an oracle read). Must run
		// AFTER processMarketSample so the impact mid / premium
		// average reflect this block's data.
		k.refreshMarkPrice(ctx, m.MarketIndex, now)
		return false
	}); err != nil {
		return err
	}
	settleEvery := params.FundingPeriodMs
	if settleEvery > 0 && (meta.LastFundingRoundTimestamp == 0 || now-meta.LastFundingRoundTimestamp >= settleEvery) {
		if err := k.SettleAllMarkets(ctx, params); err != nil {
			return err
		}
		meta.LastFundingRoundTimestamp = now
		if err := k.Metadata.Set(ctx, meta); err != nil {
			return err
		}
	}
	return nil
}

// processMarketSample refreshes impact prices and (every
// `premiumSampleIntervalMs`) appends one premium sample to
// `AggregatePremiumSum`:
//
//	premium_t = (max(0, ImpactBid - idx) - max(0, idx - ImpactAsk))
//	            * FundingRateTick / idx
//
// The sample is skipped when either side of the book lacks the per-market
// impact notional (a missing impact_bid/ask would otherwise pin the premium
// near -100%). Mark price is not written here; `refreshMarkPrice` runs
// after this and consumes the refreshed impact cache.
func (k Keeper) processMarketSample(ctx context.Context, marketIdx uint32, now int64, params types.ParamsAlias) {
	d, err := k.marketKeeper.GetMarketDetails(ctx, marketIdx)
	if err != nil {
		return
	}
	// Per-market 1-minute throttle; `LastPremiumSampleTimestamp == 0` admits
	// the first sample on a fresh market.
	if d.LastPremiumSampleTimestamp != 0 && now-d.LastPremiumSampleTimestamp < premiumSampleIntervalMs {
		return
	}

	bidImp, err := k.bookKeeper.ComputeImpactPrice(ctx, marketIdx, false)
	if err != nil {
		return
	}
	askImp, err := k.bookKeeper.ComputeImpactPrice(ctx, marketIdx, true)
	if err != nil {
		return
	}
	d.ImpactBidPrice = bidImp
	d.ImpactAskPrice = askImp
	if bidImp != 0 && askImp != 0 {
		// Floor of the mean. impact_bid floors, impact_ask ceils in
		// ComputeImpactPrice, so flooring the sum keeps the mid
		// conservative.
		d.ImpactPrice = uint32((uint64(bidImp) + uint64(askImp)) / 2)
	} else {
		// One side drained: clear so consumers don't pick up a
		// half-zero value.
		d.ImpactPrice = 0
	}

	// On oracle failure persist the refreshed impact cache (for
	// observability) but do not advance LastPremiumSampleTimestamp, so the
	// next block retries immediately.
	px, err := k.oracleKeeper.GetPrice(ctx, marketIdx)
	if err != nil {
		k.mustSetMarketDetails(ctx, d)
		return
	}
	d.IndexPrice = px.IndexPrice

	if bidImp == 0 || askImp == 0 || d.IndexPrice == 0 {
		d.LastPremiumSampleTimestamp = now
		k.mustSetMarketDetails(ctx, d)
		return
	}

	idx := int64(d.IndexPrice)
	posPart := int64(0)
	if int64(bidImp) > idx {
		posPart = int64(bidImp) - idx
	}
	negPart := int64(0)
	if idx > int64(askImp) {
		negPart = idx - int64(askImp)
	}
	// premium_t = ((max(0, IB-idx) - max(0, idx-IA)) * TICK) / idx.
	// math.Int is used so impact prices near uint32 max cannot overflow
	// int64 in the (posPart-negPart)*TICK step.
	premium := math.NewInt(posPart - negPart).
		Mul(math.NewInt(perptypes.FundingRateTick)).
		Quo(math.NewInt(idx))

	// Cap samples per window so a runaway tick rate cannot destabilize
	// the clamp (default cap matches the expected ~60 samples/hour).
	if params.MaxPremiumSampleCount == 0 ||
		d.TotalPremiumSamples < params.MaxPremiumSampleCount {
		d.AggregatePremiumSum = d.AggregatePremiumSum.Add(premium)
		d.TotalPremiumSamples++
	}
	d.LastPremiumSampleTimestamp = now
	k.mustSetMarketDetails(ctx, d)
}

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

// clampInt64 clamps v into [lo, hi]. Caller guarantees lo <= hi.
func clampInt64(v, lo, hi int64) int64 {
	if v < lo {
		return lo
	}
	if v > hi {
		return hi
	}
	return v
}

// clampUint32 clamps `v` into the uint32 domain.
func clampUint32(v int64) uint32 {
	if v < 0 {
		return 0
	}
	const maxU32 = int64(1<<32 - 1)
	if v > maxU32 {
		return uint32(maxU32)
	}
	return uint32(v)
}

// median3Uint32 returns the median of three uint32 inputs.
func median3Uint32(a, b, c uint32) uint32 {
	xs := [3]uint32{a, b, c}
	sort.Slice(xs[:], func(i, j int) bool { return xs[i] < xs[j] })
	return xs[1]
}

// SettleAllMarkets converts each market's aggregate_premium_sum into a clamped
// funding rate and advances `FundingRatePrefixSum` by `mark_price * rate`.
// Markets settle independently on the global funding boundary; per-market
// store-write failures panic (see `mustSetMarketDetails`).
func (k Keeper) SettleAllMarkets(ctx context.Context, params types.ParamsAlias) error {
	return k.marketKeeper.IterateMarkets(ctx, func(m markettypes.Market) bool {
		if m.MarketType != perptypes.MarketTypePerps || m.Status != perptypes.MarketStatusActive {
			return false
		}
		k.settleMarket(ctx, m.MarketIndex, params)
		return false
	})
}

// settleMarket applies the funding-rate formula to one market:
//
//	premium             = aggregate_premium_sum / total_premium_samples
//	smallClampedPremium = premium + clamp(interestRate - premium, ±SmallClamp)
//	rate                = clamp(smallClampedPremium, ±BigClamp) / FundingPeriodDivisor
//
// The 1-hour rate is then folded into the cumulative prefix sum as
// `mark_price * rate`. `SettlePositionFunding` later applies
// `position * delta_prefix_sum / FundingRateTick`, which reduces to
// `position * markPrice * rate / FundingRateTick` -- exactly the funding
// payment definition `funding = position * markPrice * fundingRate`.
//
// Note: `mark_price` is read from `MarketDetails.MarkPrice`, which the
// per-block `refreshMarkPrice` recomputes as median(impact_price,
// index + ema(clamp(impact-idx, ±idx/200)), oracle_mark). The funding
// settlement path itself does not touch the oracle: rate computation
// depends only on the accumulated per-minute premiums + governance
// clamps + interest rate.
//
// Invariant: `TotalPremiumSamples > 0` ⇒ at least one in-window
// `processMarketSample` succeeded ⇒ `d.MarkPrice > 0`. The early-return on
// `TotalPremiumSamples == 0` keeps degenerate cases out of the math.
func (k Keeper) settleMarket(ctx context.Context, marketIdx uint32, params types.ParamsAlias) {
	d, err := k.marketKeeper.GetMarketDetails(ctx, marketIdx)
	if err != nil {
		return
	}
	if d.TotalPremiumSamples == 0 {
		return
	}
	avg := d.AggregatePremiumSum.Quo(math.NewInt(int64(d.TotalPremiumSamples)))
	ir := math.NewInt(int64(d.InterestRate))
	smallClampMag := math.NewInt(int64(d.FundingClampSmall))
	bigClampMag := math.NewInt(int64(d.FundingClampBig))

	correction := clampInt(ir.Sub(avg), smallClampMag.Neg(), smallClampMag)
	smallClamped := avg.Add(correction)
	bigClamped := clampInt(smallClamped, bigClampMag.Neg(), bigClampMag)

	divisor := params.FundingPeriodDivisor
	if divisor <= 0 {
		divisor = 1
	}
	// Per-round rate: divide the 8-hour-scale clamped premium by the
	// configured divisor (default 8) so the cumulative funding charged
	// over `divisor` rounds matches the spec's full clamp magnitude.
	rate := bigClamped.Quo(math.NewInt(divisor))

	// Prefix-sum increment encodes the markPrice of *this* round so positions
	// settled later see `pos * mark_t * rate_t` per round, even when markPrice
	// changes between rounds.
	inc := math.NewInt(int64(d.MarkPrice)).Mul(rate)
	d.FundingRatePrefixSum = d.FundingRatePrefixSum.Add(inc)
	d.AggregatePremiumSum = math.ZeroInt()
	d.TotalPremiumSamples = 0
	k.mustSetMarketDetails(ctx, d)
}

// mustSetMarketDetails persists the runtime market details and panics on
// failure. The market keeper writes the chain's runtime KV store; a write
// failure indicates state-machine corruption (out-of-disk, store layer bug,
// etc.) and there is no safe path to continue producing blocks with stale
// in-memory state.
func (k Keeper) mustSetMarketDetails(ctx context.Context, d markettypes.MarketDetails) {
	if err := k.marketKeeper.SetMarketDetails(ctx, d); err != nil {
		panic(fmt.Errorf("funding: persist market %d details: %w", d.MarketIndex, err))
	}
}

func clampInt(v, lo, hi math.Int) math.Int {
	if v.LT(lo) {
		return lo
	}
	if v.GT(hi) {
		return hi
	}
	return v
}
