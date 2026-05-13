package keeper

import (
	"context"

	sdk "github.com/cosmos/cosmos-sdk/types"

	perptypes "github.com/perpdex/perpdex-l1/types"
	markettypes "github.com/perpdex/perpdex-l1/x/market/types"
)

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
		// swallowed; the next block retries.
		k.processMarketSample(ctx, m.MarketIndex, now, params)
		// Must run AFTER processMarketSample so the refreshed impact
		// mid feeds into the markPrice median.
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
