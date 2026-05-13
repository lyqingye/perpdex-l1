// settle_market_test.go covers `settleMarket` / `SettleAllMarkets` —
// the per-round settlement path that fires once `FundingPeriodMs`
// elapses since the last `LastFundingRoundTimestamp`. The tests pin:
//
//   - settlement uses the cached MarketDetails.MarkPrice and ignores
//     a stale oracle at settle time
//   - settlement closes every market independently (one market's
//     oracle stale ⇒ that market still settles from its cache)
//   - the canonical premium → correction → small-clamp → big-clamp →
//     rate → prefix-sum formula
//   - empty windows leave the prefix sum untouched
//   - settle never calls the oracle directly; only `refreshMarkPrice`
//     does (structural invariant)
package tests

import (
	"testing"

	"github.com/stretchr/testify/require"

	"cosmossdk.io/math"

	perptypes "github.com/perpdex/perpdex-l1/types"
	fundingtypes "github.com/perpdex/perpdex-l1/x/funding/types"
	markettypes "github.com/perpdex/perpdex-l1/x/market/types"
	oracletypes "github.com/perpdex/perpdex-l1/x/oracle/types"
)

// TestSettleMarket_UsesCachedMarkAndIgnoresOracleStaleness verifies that
// settlement does NOT depend on a fresh oracle read: rate computation only
// needs the accumulated samples + InterestRate + clamps, and the mark price
// for the prefix-sum increment is read from MarketDetails.MarkPrice (cached
// during the most recent successful processMarketSample). A stale oracle at
// settle time MUST NOT abort the round nor preserve cross-window data.
func TestSettleMarket_UsesCachedMarkAndIgnoresOracleStaleness(t *testing.T) {
	oldRoundTs := int64(1_699_996_399_999)
	mk := &stubMarket{
		markets: map[uint32]markettypes.Market{
			1: {MarketIndex: 1, MarketType: perptypes.MarketTypePerps, Status: perptypes.MarketStatusActive},
		},
		details: map[uint32]markettypes.MarketDetails{
			1: {
				MarketIndex:          1,
				MarkPrice:            50_000, // cached by a previous successful sample
				IndexPrice:           49_500,
				FundingRatePrefixSum: math.ZeroInt(),
				AggregatePremiumSum:  math.NewInt(10_101 * 60),
				TotalPremiumSamples:  60,
				FundingClampSmall:    500,
				FundingClampBig:      40_000,
			},
		},
	}
	k, ctx := newFundingKeeper(
		t,
		mk,
		stubOracle{err: oracletypes.ErrStalePrice.Wrap("stale fixture")},
		stubBook{},
	)
	require.NoError(t, k.Metadata.Set(ctx, fundingtypes.FundingMetadata{
		LastFundingRoundTimestamp: oldRoundTs,
	}))

	require.NoError(t, k.BeginBlocker(ctx), "stale oracle must not abort begin-block")
	got := mk.details[1]
	require.EqualValues(t, 60_000_000, got.FundingRatePrefixSum.Int64(),
		"settle must proceed with cached mark even when oracle is stale")
	require.True(t, got.AggregatePremiumSum.IsZero(), "samples must be cleared after settle")
	require.EqualValues(t, 0, got.TotalPremiumSamples)
	meta, metaErr := k.Metadata.Get(ctx)
	require.NoError(t, metaErr)
	require.EqualValues(t, ctx.BlockTime().UnixMilli(), meta.LastFundingRoundTimestamp)
}

// TestSettleAllMarkets_AllMarketsSettleIndependentlyOfOracle covers the
// per-round behaviour for two markets: even when one market's oracle is
// stale at settle time, both still close the round using their cached mark
// prices and clear their samples for the next window.
func TestSettleAllMarkets_AllMarketsSettleIndependentlyOfOracle(t *testing.T) {
	oldRoundTs := int64(1_699_996_399_999)
	mk := &stubMarket{
		markets: map[uint32]markettypes.Market{
			1: {MarketIndex: 1, MarketType: perptypes.MarketTypePerps, Status: perptypes.MarketStatusActive},
			2: {MarketIndex: 2, MarketType: perptypes.MarketTypePerps, Status: perptypes.MarketStatusActive},
		},
		details: map[uint32]markettypes.MarketDetails{
			1: {
				MarketIndex:          1,
				MarkPrice:            50_000,
				IndexPrice:           49_500,
				FundingRatePrefixSum: math.ZeroInt(),
				AggregatePremiumSum:  math.NewInt(10_101 * 60),
				TotalPremiumSamples:  60,
				FundingClampSmall:    500,
				FundingClampBig:      40_000,
			},
			2: {
				MarketIndex:          2,
				MarkPrice:            50_000, // cached from a prior successful sample
				IndexPrice:           49_500,
				FundingRatePrefixSum: math.ZeroInt(),
				AggregatePremiumSum:  math.NewInt(20_000 * 60),
				TotalPremiumSamples:  60,
				FundingClampSmall:    500,
				FundingClampBig:      40_000,
			},
		},
	}
	k, ctx := newFundingKeeper(
		t,
		mk,
		stubOracle{
			prices: map[uint32]oracletypes.OraclePrice{
				1: {MarketIndex: 1, IndexPrice: 49_500, MarkPrice: 50_000},
			},
			errs: map[uint32]error{
				2: oracletypes.ErrStalePrice.Wrap("stale fixture"),
			},
		},
		stubBook{},
	)
	require.NoError(t, k.Metadata.Set(ctx, fundingtypes.FundingMetadata{
		LastFundingRoundTimestamp: oldRoundTs,
	}))

	require.NoError(t, k.BeginBlocker(ctx))

	// Market 1: oracle fresh. refreshMarkPrice runs before settle and
	// re-derives MarkPrice. impact=0 freezes premium_raw to 0; the
	// first refresh reseeds EMA to 0; price_1 = idx = 49_500. impact=0
	// triggers the mean fallback ⇒ mean(price_1, price_2) =
	// mean(49500, 50000) = 49_750. inc = 49_750 * 1200 = 59_700_000.
	settled := mk.details[1]
	require.EqualValues(t, 59_700_000, settled.FundingRatePrefixSum.Int64())
	require.True(t, settled.AggregatePremiumSum.IsZero())
	require.EqualValues(t, 0, settled.TotalPremiumSamples)

	// Market 2: oracle stale at settle but cached MarkPrice is still 50_000;
	// settle must complete using the cached mark and clear the window.
	settled2 := mk.details[2]
	avg2 := int64(20_000)
	corr2 := int64(-500) // clamp(0 - 20_000, ±500) = -500
	rate2 := (avg2 + corr2) / 8
	expected2 := int64(settled2.MarkPrice) * rate2
	require.EqualValues(t, expected2, settled2.FundingRatePrefixSum.Int64(),
		"market 2 must settle using cached MarkPrice even when oracle is stale")
	require.True(t, settled2.AggregatePremiumSum.IsZero())
	require.EqualValues(t, 0, settled2.TotalPremiumSamples)

	meta, metaErr := k.Metadata.Get(ctx)
	require.NoError(t, metaErr)
	require.EqualValues(t, ctx.BlockTime().UnixMilli(), meta.LastFundingRoundTimestamp)
}

// TestSettleMarket_Formula pins down the clamp/divisor logic and the
// mark*rate prefix-sum convention.
//
// BeginBlocker now runs `refreshMarkPrice` BEFORE settling, recomputing
// `MarkPrice` as median(impact_price, index + ema(clamp(impact-idx,
// ±idx/200)), oracle_mark). In this fixture the orderbook stub reports
// both sides ok=false so d.ImpactPrice stays at 0; impact=0 freezes
// premium_raw=0 and the medianer falls back to mean(price_1, price_2):
//
//	premium_raw = 0  (impact=0)
//	premium_ema = 0  (first call reseeds to raw)
//	price_1 = clampUint32(49500 + 0) = 49500
//	price_2 = oracle_mark = 50000
//	mark    = mean(price_1, price_2) = 49750  (impact=0 ⇒ mean fallback)
//
// Settle then runs with:
//
//	premium=10101, ir=0, SmallClamp=500, BigClamp=40000, divisor=8, mark=49750
//	correction = clamp(0 - 10101, -500, +500) = -500
//	smallClamped = 10101 + (-500) = 9601
//	bigClamped = clamp(9601, -40000, +40000) = 9601
//	rate = 9601 / 8 = 1200 (truncated)
//	prefix increment = mark * rate = 49_750 * 1200 = 59_700_000
func TestSettleMarket_Formula(t *testing.T) {
	mk := &stubMarket{
		markets: map[uint32]markettypes.Market{
			1: {MarketIndex: 1, MarketType: perptypes.MarketTypePerps, Status: perptypes.MarketStatusActive},
		},
		details: map[uint32]markettypes.MarketDetails{
			1: {
				MarketIndex:          1,
				MarkPrice:            50_000,
				IndexPrice:           49_500,
				FundingRatePrefixSum: math.ZeroInt(),
				AggregatePremiumSum:  math.NewInt(10_101 * 60), // avg = 10101
				TotalPremiumSamples:  60,
				FundingClampSmall:    500,
				FundingClampBig:      40_000,
				InterestRate:         0,
			},
		},
	}
	ok := stubOracle{price: oracletypes.OraclePrice{IndexPrice: 49_500, MarkPrice: 50_000}}
	// Make ComputeImpactPrice return false so the begin-blocker pre-settle
	// sample step does not mutate the aggregate we just primed.
	bk := stubBook{}

	k, ctx := newFundingKeeper(t, mk, ok, bk)
	// Force the settle branch by stepping past `FundingPeriodMs`.
	require.NoError(t, k.Metadata.Set(ctx, fundingtypes.FundingMetadata{
		LastFundingRoundTimestamp: ctx.BlockTime().UnixMilli() - perptypes.FundingPeriod - 1,
	}))
	require.NoError(t, k.BeginBlocker(ctx))

	got := mk.details[1]
	// Pin the refreshMarkPrice path: refreshMarkPrice must have
	// rewritten MarkPrice 50_000 → 49_750 BEFORE settleMarket
	// consumed it. If a future refactor accidentally moves the
	// refresh after settle (or skips it entirely), this assertion
	// catches the regression before the prefix-sum check fires below.
	require.EqualValues(t, 49_750, got.MarkPrice,
		"refreshMarkPrice must have re-derived MarkPrice from the median pipeline before settle")
	require.EqualValues(t, 59_700_000, got.FundingRatePrefixSum.Int64(),
		"prefix sum must grow by mark * rate (with rate = clamped/divisor)")
	require.True(t, got.AggregatePremiumSum.IsZero(), "settle must reset aggregate")
	require.EqualValues(t, 0, got.TotalPremiumSamples, "settle must reset sample count")
}

// TestSettleMarket_NoSamples leaves the prefix sum untouched when the
// settlement window collected no samples (all blocks fell within the
// throttle, or the market just came online).
func TestSettleMarket_NoSamples(t *testing.T) {
	mk := &stubMarket{
		markets: map[uint32]markettypes.Market{
			1: {MarketIndex: 1, MarketType: perptypes.MarketTypePerps, Status: perptypes.MarketStatusActive},
		},
		details: map[uint32]markettypes.MarketDetails{
			1: {
				MarketIndex:          1,
				FundingRatePrefixSum: math.NewInt(123),
				AggregatePremiumSum:  math.ZeroInt(),
				TotalPremiumSamples:  0,
			},
		},
	}
	ok := stubOracle{price: oracletypes.OraclePrice{IndexPrice: 49_500, MarkPrice: 50_000}}
	bk := stubBook{}

	k, ctx := newFundingKeeper(t, mk, ok, bk)
	// Force the settle branch by stepping past `FundingPeriodMs`.
	require.NoError(t, k.Metadata.Set(ctx, fundingtypes.FundingMetadata{
		LastFundingRoundTimestamp: ctx.BlockTime().UnixMilli() - perptypes.FundingPeriod - 1,
	}))
	require.NoError(t, k.BeginBlocker(ctx))
	require.EqualValues(t, 123, mk.details[1].FundingRatePrefixSum.Int64())
}

// TestSettleMarket_NoOracleCallInSettlePath pins down the structural
// invariant that `settleMarket` must not call the oracle. We:
//
//  1. Pre-prime the market with TotalPremiumSamples=60 + cached MarkPrice so
//     the settle path has everything it needs.
//  2. Set `d.LastPremiumSampleTimestamp = now` so the per-minute throttle drops
//     `processMarketSample` before it can touch the oracle.
//  3. Step past `FundingPeriodMs` so the settle branch fires.
//  4. Run BeginBlocker with a spying oracle that always errors out.
//
// Note: `refreshMarkPrice` (which runs every block before settle) DOES
// call the oracle. So the spy will see exactly one call — from
// refreshMarkPrice — and zero calls from settleMarket. Because the spy
// returns an error, refreshMarkPrice bails out early without mutating
// d.MarkPrice, so the cached `50_000` survives into settleMarket and
// the prefix increment matches the expected value.
func TestSettleMarket_NoOracleCallInSettlePath(t *testing.T) {
	mk := &stubMarket{
		markets: map[uint32]markettypes.Market{
			1: {MarketIndex: 1, MarketType: perptypes.MarketTypePerps, Status: perptypes.MarketStatusActive},
		},
		details: map[uint32]markettypes.MarketDetails{
			1: {
				MarketIndex:          1,
				MarkPrice:            50_000, // cached from a previous sample
				IndexPrice:           49_500,
				FundingRatePrefixSum: math.ZeroInt(),
				AggregatePremiumSum:  math.NewInt(10_101 * 60),
				TotalPremiumSamples:  60,
				FundingClampSmall:    500,
				FundingClampBig:      40_000,
			},
		},
	}
	spy := &spyOracle{}
	bk := stubBook{}

	k, ctx := newFundingKeeper(t, mk, spy, bk)
	mk.details[1] = func() markettypes.MarketDetails {
		d := mk.details[1]
		// Pin LastPremiumSampleTimestamp to now so the 1-minute throttle drops
		// processMarketSample before it reaches GetPrice.
		d.LastPremiumSampleTimestamp = ctx.BlockTime().UnixMilli()
		return d
	}()
	require.NoError(t, k.Metadata.Set(ctx, fundingtypes.FundingMetadata{
		LastFundingRoundTimestamp: ctx.BlockTime().UnixMilli() - perptypes.FundingPeriod - 1,
	}))

	require.NoError(t, k.BeginBlocker(ctx))
	// Exactly one oracle call: refreshMarkPrice. processMarketSample
	// was throttled out and settleMarket reads only the cached
	// MarkPrice -- no oracle call from the settle path itself.
	require.Equal(t, 1, spy.calls[1],
		"only refreshMarkPrice may query oracle (1 call); settleMarket itself must read only cached state")

	got := mk.details[1]
	require.EqualValues(t, 60_000_000, got.FundingRatePrefixSum.Int64())
	require.True(t, got.AggregatePremiumSum.IsZero())
	require.EqualValues(t, 0, got.TotalPremiumSamples)
}
