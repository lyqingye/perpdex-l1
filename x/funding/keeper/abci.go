package keeper

import (
	"context"
	"errors"
	"strconv"

	"cosmossdk.io/math"

	sdk "github.com/cosmos/cosmos-sdk/types"

	perptypes "github.com/perpdex/perpdex-l1/types"
	"github.com/perpdex/perpdex-l1/x/funding/types"
	markettypes "github.com/perpdex/perpdex-l1/x/market/types"
	oracletypes "github.com/perpdex/perpdex-l1/x/oracle/types"
)

// premiumSampleIntervalMs is the per-market spacing between two consecutive
// premium samples. Lighter samples once a minute (60 samples per hour); we
// match that on a per-market basis using `MarketDetails.LastUpdatedTimestamp`.
const premiumSampleIntervalMs = perptypes.MinuteInMs

// BeginBlocker drives the per-market funding pipeline:
//
//  1. For every active perp market that has not been sampled in the last
//     minute, recompute `ImpactBidPrice` / `ImpactAskPrice` (VWAP over
//     `ImpactUSDCAmount` of resting depth) and push a Lighter-style premium
//     sample into `AggregatePremiumSum`.
//  2. Once `now - LastFundingRoundTimestamp >= FundingPeriodMs` (one hour by
//     default), close the funding round: average the samples, apply the
//     double clamp, divide by `FundingPeriodDivisor` to obtain the per-round
//     rate, and bump `FundingRatePrefixSum` by `mark_price * rate` so
//     `SettlePositionFunding` can compute `pos * mark * rate` from the
//     prefix-sum delta alone.
//
// Per-market errors are surfaced via dedicated events so observability picks
// up individual failures. Oracle-unavailable cases skip that market's current
// work; unexpected internal errors are returned.
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
	var firstErr error
	if err := k.marketKeeper.IterateMarkets(ctx, func(m markettypes.Market) bool {
		if m.MarketType != perptypes.MarketTypePerps || m.Status != perptypes.MarketStatusActive {
			return false
		}
		if err := k.processMarketSample(ctx, m.MarketIndex, now, params); err != nil {
			sdkCtx.EventManager().EmitEvent(sdk.NewEvent(
				"funding_sample_error",
				sdk.NewAttribute("market_index", strconv.FormatUint(uint64(m.MarketIndex), 10)),
				sdk.NewAttribute("err", err.Error()),
			))
			if firstErr == nil {
				firstErr = err
			}
		}
		return false
	}); err != nil {
		return err
	}
	settleEvery := params.FundingPeriodMs
	if settleEvery > 0 && (meta.LastFundingRoundTimestamp == 0 || now-meta.LastFundingRoundTimestamp >= settleEvery) {
		if err := k.SettleAllMarkets(ctx, params); err != nil && firstErr == nil {
			firstErr = err
		}
		meta.LastFundingRoundTimestamp = now
		if err := k.Metadata.Set(ctx, meta); err != nil {
			return err
		}
	}
	return firstErr
}

// processMarketSample updates the running aggregate_premium_sum for a market
// using Lighter's premium formula:
//
//	premium_t = ( max(0, ImpactBid - index) - max(0, index - ImpactAsk) ) / index
//
// Sampling is throttled to once every `premiumSampleIntervalMs` per market
// (see `MarketDetails.LastUpdatedTimestamp`). When either side of the book
// has insufficient depth to absorb `ImpactUSDCAmount` of quote we skip the
// sample entirely instead of feeding a degenerate `ImpactBid=0` /
// `ImpactAsk=0` into the formula (which would otherwise drive the premium to
// roughly -100%).
func (k Keeper) processMarketSample(ctx context.Context, marketIdx uint32, now int64, params types.ParamsAlias) error {
	d, err := k.marketKeeper.GetMarketDetails(ctx, marketIdx)
	if err != nil {
		return err
	}
	// Per-market 1-minute throttle. `LastUpdatedTimestamp == 0` means we
	// have not sampled yet (fresh market or post-genesis), so always
	// admit the first sample.
	if d.LastUpdatedTimestamp != 0 && now-d.LastUpdatedTimestamp < premiumSampleIntervalMs {
		return nil
	}

	// Refresh impact prices using the orderbook governance's impact
	// notional so funding stays in lock-step with the public ImpactPrice
	// query and stays governance-tunable.
	impactNotional, err := k.bookKeeper.ImpactUsdcAmount(ctx)
	if err != nil {
		return err
	}
	bidImp, bidOk, err := k.bookKeeper.ComputeImpactPrice(ctx, marketIdx, false, impactNotional)
	if err != nil {
		return err
	}
	askImp, askOk, err := k.bookKeeper.ComputeImpactPrice(ctx, marketIdx, true, impactNotional)
	if err != nil {
		return err
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
	// aggregation missed this market for long enough to go stale, emit an
	// observable event and skip the sample instead of falling back to cached
	// MarketDetails index/mark values.
	px, err := k.oracleKeeper.GetPrice(ctx, marketIdx)
	if err != nil {
		sdk.UnwrapSDKContext(ctx).EventManager().EmitEvent(sdk.NewEvent(
			"funding_oracle_price_error",
			sdk.NewAttribute("market_index", strconv.FormatUint(uint64(marketIdx), 10)),
			sdk.NewAttribute("err", err.Error()),
		))
		if isOracleUnavailable(err) {
			return k.marketKeeper.SetMarketDetails(ctx, d)
		}
		return err
	}
	d.IndexPrice = px.IndexPrice
	d.MarkPrice = px.MarkPrice

	// Skip the sample when either side cannot absorb ImpactUSDCAmount of
	// quote (insufficient depth). Otherwise `max(0, idx - 0) = idx` would
	// peg the premium at roughly -100% and corrupt the running average.
	if !bidOk || !askOk || d.IndexPrice == 0 {
		d.LastUpdatedTimestamp = now
		return k.marketKeeper.SetMarketDetails(ctx, d)
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
	premium := (posPart - negPart) * perptypes.FundingRateTick / idx

	// Defense-in-depth: cap the per-window sample count so a runaway tick
	// rate cannot destabilize the clamp. With 1-minute sampling and a
	// 1-hour window we expect ~60 samples; the cap (default 60) matches.
	if params.MaxPremiumSampleCount == 0 ||
		d.TotalPremiumSamples < params.MaxPremiumSampleCount {
		d.AggregatePremiumSum += premium
		d.TotalPremiumSamples++
	}
	d.LastUpdatedTimestamp = now
	return k.marketKeeper.SetMarketDetails(ctx, d)
}

// SettleAllMarkets converts each market's aggregate_premium_sum into a clamped
// funding rate and advances `FundingRatePrefixSum` by `mark_price * rate`.
// Markets settle independently on the global funding boundary: one market with
// a stale oracle skips and clears its current window without blocking other
// markets from closing the same round.
func (k Keeper) SettleAllMarkets(ctx context.Context, params types.ParamsAlias) error {
	sdkCtx := sdk.UnwrapSDKContext(ctx)
	var firstErr error
	if err := k.marketKeeper.IterateMarkets(ctx, func(m markettypes.Market) bool {
		if m.MarketType != perptypes.MarketTypePerps || m.Status != perptypes.MarketStatusActive {
			return false
		}
		if err := k.settleMarket(ctx, m.MarketIndex, params); err != nil {
			sdkCtx.EventManager().EmitEvent(sdk.NewEvent(
				"funding_settle_error",
				sdk.NewAttribute("market_index", strconv.FormatUint(uint64(m.MarketIndex), 10)),
				sdk.NewAttribute("err", err.Error()),
			))
			if !isOracleUnavailable(err) && firstErr == nil {
				firstErr = err
			}
		}
		return false
	}); err != nil {
		return err
	}
	return firstErr
}

// settleMarket applies the Lighter funding-rate formula to one market:
//
//	premium             = aggregate_premium_sum / total_premium_samples
//	smallClampedPremium = premium + clamp(interestRate - premium, ±SmallClamp)
//	rate                = clamp(smallClampedPremium, ±BigClamp) / FundingPeriodDivisor
//
// The 1-hour rate is then folded into the cumulative prefix sum as
// `mark_price * rate`. `SettlePositionFunding` later applies
// `position * delta_prefix_sum / FundingRateTick`, which reduces to
// `position * mark * rate / FundingRateTick` -- exactly the Lighter funding
// payment definition `funding = position * mark * fundingRate`.
func (k Keeper) settleMarket(ctx context.Context, marketIdx uint32, params types.ParamsAlias) error {
	d, err := k.marketKeeper.GetMarketDetails(ctx, marketIdx)
	if err != nil {
		return err
	}
	if d.FundingRatePrefixSum.IsNil() {
		d.FundingRatePrefixSum = math.ZeroInt()
	}
	if d.TotalPremiumSamples == 0 {
		return nil
	}
	px, err := k.oracleKeeper.GetPrice(ctx, marketIdx)
	if err != nil {
		if isOracleUnavailable(err) {
			d.AggregatePremiumSum = 0
			d.TotalPremiumSamples = 0
			return k.marketKeeper.SetMarketDetails(ctx, d)
		}
		return err
	}
	d.IndexPrice = px.IndexPrice
	d.MarkPrice = px.MarkPrice
	avg := d.AggregatePremiumSum / int64(d.TotalPremiumSamples)
	ir := int64(d.InterestRate)
	smallClampMag := int64(d.FundingClampSmall)
	bigClampMag := int64(d.FundingClampBig)

	// Lighter small clamp: the correction term is `clamp(ir - premium, ±SmallClamp)`.
	correction := clampInt64(ir-avg, -smallClampMag, smallClampMag)
	smallClamped := avg + correction
	bigClamped := clampInt64(smallClamped, -bigClampMag, bigClampMag)

	divisor := params.FundingPeriodDivisor
	if divisor <= 0 {
		divisor = 1
	}
	// Per-round rate: divide the 8-hour-scale clamped premium by the
	// configured divisor (default 8) so the cumulative funding charged
	// over `divisor` rounds matches the spec's full clamp magnitude.
	rate := bigClamped / divisor

	// Prefix-sum increment encodes the mark of *this* round so positions
	// settled later see `pos * mark_t * rate_t` per round, even when mark
	// changes between rounds.
	inc := math.NewInt(int64(px.MarkPrice)).Mul(math.NewInt(rate))
	d.FundingRatePrefixSum = d.FundingRatePrefixSum.Add(inc)
	d.AggregatePremiumSum = 0
	d.TotalPremiumSamples = 0
	return k.marketKeeper.SetMarketDetails(ctx, d)
}

func clampInt64(v, lo, hi int64) int64 {
	if v < lo {
		return lo
	}
	if v > hi {
		return hi
	}
	return v
}

func isOracleUnavailable(err error) bool {
	return errors.Is(err, oracletypes.ErrPriceNotFound) ||
		errors.Is(err, oracletypes.ErrStalePrice)
}
