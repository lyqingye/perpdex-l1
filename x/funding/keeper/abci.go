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
// per-market basis using `MarketDetails.LastUpdatedTimestamp`.
const premiumSampleIntervalMs = perptypes.MinuteInMs

// BeginBlocker drives the per-market funding pipeline. Each block runs two
// concentric loops over the active perp markets:
//
//  1. Mark-price refresh (every block): combine the cached impact mid, the
//     index price + running premium average, and the oracle weighted-median
//     mark into a 3-input median and write the result back to
//     `MarketDetails.MarkPrice`. This is the chain's authoritative mark
//     price: x/risk, x/trade and x/matching read from here, not from oracle.
//     See `refreshMarkPrice` for the derivation.
//
//  2. Premium sample (1-minute throttle): refresh `ImpactBidPrice` /
//     `ImpactAskPrice` / `ImpactPrice` from the live orderbook and push a
//     premium sample into `AggregatePremiumSum`, following the official
//     formula
//     premium_t = (max(0, IB-idx) - max(0, idx-IA)) * FundingRateTick / idx.
//
//  3. Funding settlement (every `FundingPeriodMs`, default 1 hour): close
//     the round, average the samples, apply the double clamp, divide by
//     `FundingPeriodDivisor` to obtain the per-round rate, and bump
//     `FundingRatePrefixSum` by `mark_price * rate`. The settlement uses
//     the freshly-refreshed `MarkPrice` from step (1).
//
// Per-market business errors (oracle stale, single-sided depth, etc.) are
// expected steady-state events and are swallowed silently so a transient
// pricing hiccup on one market does not abort the whole begin-block.
// Persistence failures from `SetMarketDetails` are treated as fatal:
// `MarketKeeper` writes the runtime store and is not allowed to fail under
// normal operation, so we panic to surface state-machine corruption rather
// than continue with inconsistent in-memory data.
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
		// Refresh the authoritative mark every block (cheap: it just
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

// processMarketSample updates the running aggregate_premium_sum for a market
// using the premium formula:
//
//	premium_t = ( max(0, ImpactBid - index) - max(0, index - ImpactAsk) )
//	            * FundingRateTick / index
//
// Sampling is throttled to once every `premiumSampleIntervalMs` per market
// (see `MarketDetails.LastUpdatedTimestamp`). When either side of the book
// has insufficient depth to absorb the per-market impact notional we skip
// the sample entirely instead of feeding a degenerate `ImpactBid=0` /
// `ImpactAsk=0` into the formula (which would otherwise drive the premium
// to roughly -100%).
//
// Mark price is NOT written here; the per-block `refreshMarkPrice` runs
// after this and consumes the (possibly stale-by-up-to-one-minute) impact
// cache plus the live oracle reading.
//
// Returns nothing: oracle / orderbook errors are silently absorbed (they are
// expected steady-state events), while `SetMarketDetails` failures panic
// because the runtime store is not allowed to fail.
func (k Keeper) processMarketSample(ctx context.Context, marketIdx uint32, now int64, params types.ParamsAlias) {
	d, err := k.marketKeeper.GetMarketDetails(ctx, marketIdx)
	if err != nil {
		return
	}
	// Per-market 1-minute throttle. `LastUpdatedTimestamp == 0` means we
	// have not sampled yet (fresh market or post-genesis), so always
	// admit the first sample.
	if d.LastUpdatedTimestamp != 0 && now-d.LastUpdatedTimestamp < premiumSampleIntervalMs {
		return
	}

	// Refresh impact prices using the per-market impact notional
	// derived from MinInitialMarginFraction (see
	// x/orderbook keeper.MarketImpactNotional). The orderbook keeper
	// loads the market details internally; we no longer thread a
	// global impact_usdc_amount param through this path.
	bidImp, bidOk, err := k.bookKeeper.ComputeImpactPrice(ctx, marketIdx, false)
	if err != nil {
		return
	}
	askImp, askOk, err := k.bookKeeper.ComputeImpactPrice(ctx, marketIdx, true)
	if err != nil {
		return
	}
	d.ImpactBidPrice = bidImp
	d.ImpactAskPrice = askImp
	if bidOk && askOk {
		// Floor of the mean: impact_bid is floor-divided while
		// impact_ask is ceil-divided in ComputeImpactPrice, so the
		// floor of the sum/2 keeps the mid conservative.
		d.ImpactPrice = uint32((uint64(bidImp) + uint64(askImp)) / 2)
	} else {
		// One side drained: mid is undefined. Clear the cache so
		// neither the gRPC consumer nor the per-block median pipeline
		// pick up a half-zero value.
		d.ImpactPrice = 0
	}

	// Funding must only sample against a fresh oracle price. If oracle
	// aggregation missed this market for long enough to go stale, persist
	// the refreshed impact info (for observability) but skip the sample.
	// LastUpdatedTimestamp is intentionally NOT advanced so the next block
	// retries the oracle fetch immediately on recovery.
	px, err := k.oracleKeeper.GetPrice(ctx, marketIdx)
	if err != nil {
		k.mustSetMarketDetails(ctx, d)
		return
	}
	d.IndexPrice = px.IndexPrice

	// Skip the premium sample when either side cannot absorb the
	// impact notional. Otherwise `max(0, idx - 0) = idx` would peg
	// the premium at roughly -100% and corrupt the running average.
	if !bidOk || !askOk || d.IndexPrice == 0 {
		d.LastUpdatedTimestamp = now
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
	// premium_t = ((max(0, IB-idx) - max(0, idx-IA)) * TICK) / idx
	// Use math.Int for the intermediate product so impact prices near the
	// uint32 ceiling cannot overflow int64 in the (posPart-negPart)*TICK
	// step.
	premium := math.NewInt(posPart - negPart).
		Mul(math.NewInt(perptypes.FundingRateTick)).
		Quo(math.NewInt(idx))

	// Defense-in-depth: cap the per-window sample count so a runaway tick
	// rate cannot destabilize the clamp. With 1-minute sampling and a
	// 1-hour window we expect ~60 samples; the cap (default 60) matches.
	if params.MaxPremiumSampleCount == 0 ||
		d.TotalPremiumSamples < params.MaxPremiumSampleCount {
		d.AggregatePremiumSum = d.AggregatePremiumSum.Add(premium)
		d.TotalPremiumSamples++
	}
	d.LastUpdatedTimestamp = now
	k.mustSetMarketDetails(ctx, d)
}

// refreshMarkPrice recomputes `MarketDetails.MarkPrice` once per block
// as the median of three sources:
//
//	price_1 = index_price + premium_ema * index_price / FundingRateTick
//	          (where premium_ema = AggregatePremiumSum / TotalPremiumSamples
//	           is the running 1-hour average; equivalent to the
//	           time-weighted average of the per-minute premium samples)
//	price_2 = oracle weighted-median mark (the chain's external reference)
//	mark    = median3(impact_price, price_1, price_2)
//
// Degenerate inputs:
//   - `oracle.GetPrice` errors: leave d.MarkPrice untouched and let the
//     staleness gate in x/risk reject the read.
//   - `d.ImpactPrice == 0` (either side of the book drained): fall back to
//     `floor((price_1 + price_2) / 2)`.
//   - `d.TotalPremiumSamples == 0` (fresh market / new round just opened):
//     treat premium_ema as 0, so `price_1 = index_price`.
//
// On any successful update `MarketDetails.LastMarkPriceTimestamp` is bumped
// to `now` so the x/risk staleness gate sees a fresh mark. Note: this is a
// distinct field from `LastUpdatedTimestamp`, which is the per-minute
// premium-sampling throttle owned by `processMarketSample`.
func (k Keeper) refreshMarkPrice(ctx context.Context, marketIdx uint32, now int64) {
	d, err := k.marketKeeper.GetMarketDetails(ctx, marketIdx)
	if err != nil {
		return
	}
	// Oracle fetch failure is treated as "no fresh sample this block":
	// leave d.MarkPrice + d.LastMarkPriceTimestamp untouched so x/risk's
	// staleness gate eventually trips if the outage drags on.
	// Note: a degenerate oracle reading (IndexPrice == 0 OR MarkPrice
	// == 0) is NOT short-circuited — the fallbacks in the switch below
	// can still produce a valid mark from the remaining inputs (e.g.
	// impact_mid + price_1 even when oracle's MarkPrice is missing).
	px, err := k.oracleKeeper.GetPrice(ctx, marketIdx)
	if err != nil {
		return
	}
	if px.IndexPrice != 0 {
		d.IndexPrice = px.IndexPrice
	}

	price1 := computePrice1(px.IndexPrice, d.AggregatePremiumSum, d.TotalPremiumSamples)
	price2 := px.MarkPrice

	var mark uint32
	switch {
	case d.ImpactPrice != 0 && price1 != 0 && price2 != 0:
		mark = median3Uint32(d.ImpactPrice, price1, price2)
	case d.ImpactPrice != 0 && price1 != 0:
		mark = uint32((uint64(d.ImpactPrice) + uint64(price1)) / 2)
	case d.ImpactPrice != 0 && price2 != 0:
		mark = uint32((uint64(d.ImpactPrice) + uint64(price2)) / 2)
	case price1 != 0 && price2 != 0:
		mark = uint32((uint64(price1) + uint64(price2)) / 2)
	case d.ImpactPrice != 0:
		mark = d.ImpactPrice
	case price1 != 0:
		mark = price1
	case price2 != 0:
		mark = price2
	default:
		// All three inputs zero is a pathological state; leave the
		// previous mark in place rather than zeroing it out.
		return
	}
	d.MarkPrice = mark
	d.LastMarkPriceTimestamp = now
	k.mustSetMarketDetails(ctx, d)
}

// computePrice1 returns `index * (1 + avgPremium / FundingRateTick)`.
//
// `avgPremium = aggregatePremiumSum / sampleCount` is itself already scaled
// by `FundingRateTick` (see processMarketSample). For numerical stability
// and to avoid an extra rounding step in `avgPremium`, we collapse the math
// into a single division:
//
//	delta   = index * aggregatePremiumSum / (sampleCount * FundingRateTick)
//	price_1 = index + delta
//
// This is algebraically equivalent to `index + index * avgPremium / TICK`
// (associativity of integer multiply with one final truncating divide), but
// avoids the loss-of-precision a separate `avgPremium := aggSum/samples`
// step would introduce when `samples` does not divide `aggSum` exactly.
//
// Returns 0 when `index == 0`. When `sampleCount == 0` (no samples in the
// current window) the premium component is taken as 0, yielding price_1
// = index ("no samples → no premium").
// Results are clamped into uint32; a wildly large premium that would
// otherwise overflow the price domain returns the relevant extreme so the
// caller's median still operates on a well-defined value.
func computePrice1(index uint32, aggregatePremiumSum math.Int, sampleCount uint32) uint32 {
	if index == 0 {
		return 0
	}
	if sampleCount == 0 || aggregatePremiumSum.IsZero() {
		return index
	}
	// delta = index * aggregatePremiumSum / (sampleCount * FundingRateTick)
	delta := math.NewInt(int64(index)).
		Mul(aggregatePremiumSum).
		Quo(math.NewInt(int64(sampleCount)).Mul(math.NewInt(perptypes.FundingRateTick)))
	if delta.IsZero() {
		return index
	}
	priced := math.NewInt(int64(index)).Add(delta)
	// Negative premium can push price_1 below zero (e.g. perpetual
	// trading at a steep discount); clamp to 0 so the median input is
	// always uint32-representable.
	if priced.IsNegative() {
		return 0
	}
	const maxU32 = int64(1<<32 - 1)
	if priced.GT(math.NewInt(maxU32)) {
		return uint32(maxU32)
	}
	return uint32(priced.Int64())
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
// `position * mark * rate / FundingRateTick` -- exactly the funding
// payment definition `funding = position * mark * fundingRate`.
//
// Note: `mark_price` is read from `MarketDetails.MarkPrice`, which the
// per-block `refreshMarkPrice` recomputes as the median of impact mid,
// `index + premium_ema`, and the oracle weighted-median mark. There is
// no oracle call in this path: rate computation depends only on the
// accumulated per-minute premiums + governance clamps + interest rate.
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

	// Prefix-sum increment encodes the mark of *this* round so positions
	// settled later see `pos * mark_t * rate_t` per round, even when mark
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
