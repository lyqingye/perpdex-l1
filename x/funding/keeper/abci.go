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
	// Per-market 1-minute throttle; `LastUpdatedTimestamp == 0` admits
	// the first sample on a fresh market.
	if d.LastUpdatedTimestamp != 0 && now-d.LastUpdatedTimestamp < premiumSampleIntervalMs {
		return
	}

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
	// observability) but do not advance LastUpdatedTimestamp, so the
	// next block retries immediately.
	px, err := k.oracleKeeper.GetPrice(ctx, marketIdx)
	if err != nil {
		k.mustSetMarketDetails(ctx, d)
		return
	}
	d.IndexPrice = px.IndexPrice

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
	d.LastUpdatedTimestamp = now
	k.mustSetMarketDetails(ctx, d)
}

// refreshMarkPrice rewrites `MarketDetails.MarkPrice` once per block as
//
//	price_1 = index + premium_ema * index / FundingRateTick
//	          (premium_ema = AggregatePremiumSum / TotalPremiumSamples)
//	price_2 = oracle weighted-median mark
//	mark    = median3(impact_price, price_1, price_2)
//
// Fallbacks: oracle failure leaves MarkPrice untouched (letting x/risk's
// staleness gate eventually fire); zero impact_price degrades to the mean
// of the two non-zero inputs; zero TotalPremiumSamples sets price_1 = index.
//
// On success `LastMarkPriceTimestamp` is bumped to `now` (distinct from
// `LastUpdatedTimestamp`, which throttles `processMarketSample`).
func (k Keeper) refreshMarkPrice(ctx context.Context, marketIdx uint32, now int64) {
	d, err := k.marketKeeper.GetMarketDetails(ctx, marketIdx)
	if err != nil {
		return
	}
	// Oracle failure leaves MarkPrice/LastMarkPriceTimestamp untouched so
	// x/risk's staleness gate eventually trips. A zero IndexPrice or
	// MarkPrice from the oracle is NOT short-circuited — the switch
	// below can still derive a mark from the remaining inputs.
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
		// All three inputs zero: keep the previous mark.
		return
	}
	d.MarkPrice = mark
	d.LastMarkPriceTimestamp = now
	k.mustSetMarketDetails(ctx, d)
}

// computePrice1 returns `index + index * avgPremium / FundingRateTick`,
// where `avgPremium = aggregatePremiumSum / sampleCount` (already scaled
// by FundingRateTick — see processMarketSample). The two divisions are
// collapsed into one to avoid losing precision from the intermediate
// average:
//
//	price_1 = index + index * aggregatePremiumSum
//	                  / (sampleCount * FundingRateTick)
//
// Returns 0 when index == 0; returns index when there are no samples in
// the window. The result is clamped into uint32 so an overflowing premium
// still yields a well-defined median input.
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
	// A deep discount can push price_1 below 0; clamp so the median
	// input is always uint32-representable.
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
