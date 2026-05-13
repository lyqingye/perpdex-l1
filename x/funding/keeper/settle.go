package keeper

import (
	"context"

	"cosmossdk.io/math"

	perptypes "github.com/perpdex/perpdex-l1/types"
	"github.com/perpdex/perpdex-l1/x/funding/types"
	markettypes "github.com/perpdex/perpdex-l1/x/market/types"
)

// This file owns the once-per-funding-round settlement step. It folds the
// accumulated premium samples into a clamped funding rate and advances
// `MarketDetails.FundingRatePrefixSum`, which downstream account-level
// settlement consumes via `Keeper.SettlePositionFunding`.

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
