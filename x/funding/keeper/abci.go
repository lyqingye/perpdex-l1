package keeper

import (
	"context"
	"fmt"

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

// BeginBlocker drives the per-market funding pipeline:
//
//  1. For every active perp market that has not been sampled in the last
//     minute, recompute `ImpactBidPrice` / `ImpactAskPrice` (VWAP over
//     `ImpactUSDCAmount` of resting depth) and push a premium sample into
//     `AggregatePremiumSum`.
//  2. Once `now - LastFundingRoundTimestamp >= FundingPeriodMs` (one hour by
//     default), close the funding round: average the samples, apply the
//     double clamp, divide by `FundingPeriodDivisor` to obtain the per-round
//     rate, and bump `FundingRatePrefixSum` by `mark_price * rate` so
//     `SettlePositionFunding` can compute `pos * mark * rate` from the
//     prefix-sum delta alone.
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
//	premium_t = ( max(0, ImpactBid - index) - max(0, index - ImpactAsk) ) / index
//
// Sampling is throttled to once every `premiumSampleIntervalMs` per market
// (see `MarketDetails.LastUpdatedTimestamp`). When either side of the book
// has insufficient depth to absorb `ImpactUSDCAmount` of quote we skip the
// sample entirely instead of feeding a degenerate `ImpactBid=0` /
// `ImpactAsk=0` into the formula (which would otherwise drive the premium to
// roughly -100%).
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

	// Refresh impact prices using the orderbook governance's impact
	// notional so funding stays in lock-step with the public ImpactPrice
	// query and stays governance-tunable.
	impactNotional, err := k.bookKeeper.ImpactUsdcAmount(ctx)
	if err != nil {
		return
	}
	bidImp, bidOk, err := k.bookKeeper.ComputeImpactPrice(ctx, marketIdx, false, impactNotional)
	if err != nil {
		return
	}
	askImp, askOk, err := k.bookKeeper.ComputeImpactPrice(ctx, marketIdx, true, impactNotional)
	if err != nil {
		return
	}
	d.ImpactBidPrice = bidImp
	d.ImpactAskPrice = askImp
	if bidOk && askOk {
		d.ImpactPrice = uint32((uint64(bidImp) + uint64(askImp)) / 2)
	} else {
		// Observability only; the funding sampler does not consume the
		// mid in the new formula. Clear it so query consumers do not
		// see a stale mid when one side of the book has drained.
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
	d.MarkPrice = px.MarkPrice

	// Skip the sample when either side cannot absorb ImpactUSDCAmount of
	// quote (insufficient depth). Otherwise `max(0, idx - 0) = idx` would
	// peg the premium at roughly -100% and corrupt the running average.
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
// Note: `mark_price` is read from `MarketDetails.MarkPrice`, which
// `processMarketSample` refreshes every successful 1-minute sample. There is
// no oracle call in this path: the rate computation depends only on the
// accumulated per-minute premiums + governance clamps + interest rate, none
// of which require a fresh oracle price. The mark price used in the prefix
// increment comes from the most recent successful sample within the window.
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
