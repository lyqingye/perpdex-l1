package keeper

import (
	"context"

	"cosmossdk.io/math"

	perptypes "github.com/perpdex/perpdex-l1/types"
	"github.com/perpdex/perpdex-l1/x/funding/types"
)

// premiumSampleIntervalMs throttles premium sampling to once per minute
// per market (~60 samples/hour).
const premiumSampleIntervalMs = perptypes.MinuteInMs

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
