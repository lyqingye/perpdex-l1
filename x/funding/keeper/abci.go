package keeper

import (
	"context"
	"strconv"

	"cosmossdk.io/math"

	sdk "github.com/cosmos/cosmos-sdk/types"

	perptypes "github.com/perpdex/perpdex-l1/types"
	"github.com/perpdex/perpdex-l1/x/funding/types"
	markettypes "github.com/perpdex/perpdex-l1/x/market/types"
)

// BeginBlocker recomputes per-market impact prices, accumulates the normalized
// premium for the current funding window, and when an integer hour boundary is
// crossed, settles the funding round (double-clamp + prefix sum bump).
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
	// Per-market errors are surfaced via an event per market so observability
	// picks up individual failures, while the first error is returned so the
	// begin-blocker flow signals a degraded state. Previously errors from
	// `processMarketSample` were swallowed with `_ =`.
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
	// Settle on hour boundaries: every funding_period_ms / funding_period_divisor.
	settleEvery := params.FundingPeriodMs / params.FundingPeriodDivisor
	if settleEvery > 0 && (meta.LastFundingRoundTimestamp == 0 || now-meta.LastFundingRoundTimestamp >= settleEvery) {
		if err := k.SettleAllMarkets(ctx, params); err != nil {
			if firstErr == nil {
				firstErr = err
			}
		}
		meta.LastFundingRoundTimestamp = now
		if err := k.Metadata.Set(ctx, meta); err != nil {
			return err
		}
	}
	return firstErr
}

// processMarketSample updates the running aggregate_premium_sum for a market.
// When either side of the book is empty the impact aggregate is reset to
// zero so stale values from a previous sample cannot leak into the premium
// once the book drains.
func (k Keeper) processMarketSample(ctx context.Context, marketIdx uint32, _ int64, params types.ParamsAlias) error {
	d, err := k.marketKeeper.GetMarketDetails(ctx, marketIdx)
	if err != nil {
		return err
	}
	bid, ask, err := k.bookKeeper.BestBidAsk(ctx, marketIdx)
	if err != nil {
		return err
	}
	d.ImpactBidPrice = bid
	d.ImpactAskPrice = ask
	if bid > 0 && ask > 0 {
		d.ImpactPrice = uint32((uint64(bid) + uint64(ask)) / 2)
	} else {
		// One-sided or empty book: clear the stale impact price so we
		// don't accumulate a premium based on a trade that cannot
		// actually clear at the recorded price.
		d.ImpactPrice = 0
	}
	if px, err := k.oracleKeeper.GetPrice(ctx, marketIdx); err == nil && px.IndexPrice > 0 {
		d.IndexPrice = px.IndexPrice
		d.MarkPrice = px.MarkPrice
		// Premium = (impact_price - index_price) * tick / index_price (basis points).
		var premium int64
		if d.ImpactPrice > 0 {
			diff := int64(d.ImpactPrice) - int64(d.IndexPrice)
			premium = diff * perptypes.FundingRateTick / int64(d.IndexPrice)
		}
		// Stop accumulating once we've reached the configured sample
		// cap so a runaway tick count cannot destabilize the clamp.
		if params.MaxPremiumSampleCount == 0 ||
			d.TotalPremiumSamples < params.MaxPremiumSampleCount {
			d.AggregatePremiumSum += premium
			d.TotalPremiumSamples++
		}
	}
	d.LastUpdatedTimestamp = sdk.UnwrapSDKContext(ctx).BlockTime().UnixMilli()
	return k.marketKeeper.SetMarketDetails(ctx, d)
}

// SettleAllMarkets converts each market's aggregate_premium_sum into a clamped
// funding rate, advances the prefix sum, and resets the accumulator. Like
// the sample loop above, errors are surfaced via per-market events and the
// first error is returned so callers can react.
func (k Keeper) SettleAllMarkets(ctx context.Context, _ types.ParamsAlias) error {
	sdkCtx := sdk.UnwrapSDKContext(ctx)
	var firstErr error
	if err := k.marketKeeper.IterateMarkets(ctx, func(m markettypes.Market) bool {
		if m.MarketType != perptypes.MarketTypePerps || m.Status != perptypes.MarketStatusActive {
			return false
		}
		if err := k.settleMarket(ctx, m.MarketIndex); err != nil {
			sdkCtx.EventManager().EmitEvent(sdk.NewEvent(
				"funding_settle_error",
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
	return firstErr
}

func (k Keeper) settleMarket(ctx context.Context, marketIdx uint32) error {
	d, err := k.marketKeeper.GetMarketDetails(ctx, marketIdx)
	if err != nil {
		return err
	}
	if d.TotalPremiumSamples == 0 {
		return nil
	}
	avgPremium := d.AggregatePremiumSum / int64(d.TotalPremiumSamples)
	rate := avgPremium + int64(d.InterestRate)
	// Double clamp: small clamp first, big clamp second.
	rate = clampInt64(rate, -int64(d.FundingClampSmall), int64(d.FundingClampSmall))
	rate = clampInt64(rate, -int64(d.FundingClampBig), int64(d.FundingClampBig))

	if d.FundingRatePrefixSum.IsNil() {
		d.FundingRatePrefixSum = math.ZeroInt()
	}
	d.FundingRatePrefixSum = d.FundingRatePrefixSum.Add(math.NewInt(rate))
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
