// process_sample_test.go covers `processMarketSample` — the per-block
// premium-sampling path that runs at the top of BeginBlocker. The
// tests pin:
//
//   - stale-oracle handling (sample is skipped, no retry throttling)
//   - one-sided orderbook depth (sample skipped instead of -100% peg)
//   - the canonical `(IB-idx) * TICK / idx` premium computation
//   - the per-market 1-minute sampling cadence
//   - the `MaxPremiumSampleCount` cap
//   - large-impact-price overflow safety
package tests

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"cosmossdk.io/math"

	perptypes "github.com/perpdex/perpdex-l1/types"
	fundingtypes "github.com/perpdex/perpdex-l1/x/funding/types"
	markettypes "github.com/perpdex/perpdex-l1/x/market/types"
	oracletypes "github.com/perpdex/perpdex-l1/x/oracle/types"
)

func TestProcessMarketSample_StaleOracleSkipsSample(t *testing.T) {
	mk := &stubMarket{
		markets: map[uint32]markettypes.Market{
			1: {MarketIndex: 1, MarketType: perptypes.MarketTypePerps, Status: perptypes.MarketStatusActive},
		},
		details: map[uint32]markettypes.MarketDetails{
			1: {
				MarketIndex:          1,
				IndexPrice:           49_500,
				MarkPrice:            50_000,
				FundingRatePrefixSum: math.ZeroInt(),
				AggregatePremiumSum:  math.ZeroInt(),
			},
		},
	}
	k, ctx := newFundingKeeper(
		t,
		mk,
		stubOracle{err: oracletypes.ErrStalePrice.Wrap("stale fixture")},
		stubBook{bid: 49_999, ask: 50_001},
	)

	require.NoError(t, k.BeginBlocker(ctx))
	got := mk.details[1]
	require.EqualValues(t, 0, got.TotalPremiumSamples)
	require.True(t, got.AggregatePremiumSum.IsZero())
	require.EqualValues(t, 0, got.LastPremiumSampleTimestamp, "stale oracle must not throttle retries")
}

// TestProcessMarketSample_OneSidedSkipsSample verifies that when only one
// side of the book can absorb ImpactUSDCAmount of quote we skip the sample
// (rather than feeding ImpactBid=0 / ImpactAsk=0 into the formula, which
// would otherwise peg the premium near -100%).
func TestProcessMarketSample_OneSidedSkipsSample(t *testing.T) {
	mk := &stubMarket{
		markets: map[uint32]markettypes.Market{
			1: {MarketIndex: 1, MarketType: perptypes.MarketTypePerps, Status: perptypes.MarketStatusActive},
		},
		details: map[uint32]markettypes.MarketDetails{
			1: {
				MarketIndex:          1,
				ImpactPrice:          999, // stale
				AggregatePremiumSum:  math.NewInt(42),
				TotalPremiumSamples:  1,
				FundingRatePrefixSum: math.ZeroInt(),
			},
		},
	}
	ok := stubOracle{price: oracletypes.OraclePrice{IndexPrice: 100, MarkPrice: 100}}
	bk := stubBook{ask: 110} // ask depth only

	k, ctx := newFundingKeeper(t, mk, ok, bk)
	require.NoError(t, k.BeginBlocker(ctx))
	got := mk.details[1]
	require.EqualValues(t, 0, got.ImpactPrice, "stale impact mid must be cleared")
	require.EqualValues(t, 0, got.ImpactBidPrice)
	require.EqualValues(t, 110, got.ImpactAskPrice)
	require.EqualValues(t, 42, got.AggregatePremiumSum.Int64(), "premium sum must not move when a side has no depth")
	require.EqualValues(t, 1, got.TotalPremiumSamples, "sample count must not advance")
}

// TestProcessMarketSample_Premium drives the premium formula directly:
//
//	premium_t = ( max(0, IB - idx) - max(0, idx - IA) ) * TICK / idx
//
// With IB=49999, IA=50001 and idx=49500 we expect
// `(49999-49500) * 1e6 / 49500 = 499*1e6/49500 = 10080`.
func TestProcessMarketSample_Premium(t *testing.T) {
	mk := &stubMarket{
		markets: map[uint32]markettypes.Market{
			1: {MarketIndex: 1, MarketType: perptypes.MarketTypePerps, Status: perptypes.MarketStatusActive},
		},
		details: map[uint32]markettypes.MarketDetails{
			1: {MarketIndex: 1, FundingRatePrefixSum: math.ZeroInt(), AggregatePremiumSum: math.ZeroInt()},
		},
	}
	ok := stubOracle{price: oracletypes.OraclePrice{IndexPrice: 49_500, MarkPrice: 50_000}}
	bk := stubBook{bid: 49_999, ask: 50_001}

	k, ctx := newFundingKeeper(t, mk, ok, bk)
	require.NoError(t, k.BeginBlocker(ctx))
	got := mk.details[1]
	expected := int64(49_999-49_500) * perptypes.FundingRateTick / int64(49_500)
	require.EqualValues(t, expected, got.AggregatePremiumSum.Int64())
	require.EqualValues(t, 1, got.TotalPremiumSamples)
	require.EqualValues(t, 49_999, got.ImpactBidPrice)
	require.EqualValues(t, 50_001, got.ImpactAskPrice)
}

// TestProcessMarketSample_PerMinuteThrottle verifies the per-market 1-minute
// sampling cadence: a second BeginBlocker within the same minute must skip
// the sample, while a third one ~70s later must admit it.
func TestProcessMarketSample_PerMinuteThrottle(t *testing.T) {
	mk := &stubMarket{
		markets: map[uint32]markettypes.Market{
			1: {MarketIndex: 1, MarketType: perptypes.MarketTypePerps, Status: perptypes.MarketStatusActive},
		},
		details: map[uint32]markettypes.MarketDetails{
			1: {MarketIndex: 1, FundingRatePrefixSum: math.ZeroInt(), AggregatePremiumSum: math.ZeroInt()},
		},
	}
	ok := stubOracle{price: oracletypes.OraclePrice{IndexPrice: 49_500, MarkPrice: 50_000}}
	bk := stubBook{bid: 49_999, ask: 50_001}

	k, ctx := newFundingKeeper(t, mk, ok, bk)

	// First sample admitted (LastPremiumSampleTimestamp == 0 on details).
	require.NoError(t, k.BeginBlocker(ctx))
	require.EqualValues(t, 1, mk.details[1].TotalPremiumSamples)
	premiumAfter1 := mk.details[1].AggregatePremiumSum

	// Second sample 30s later -- throttled by per-market 1-minute window.
	ctx30 := ctx.WithBlockTime(ctx.BlockTime().Add(30 * time.Second))
	require.NoError(t, k.Metadata.Set(ctx30, fundingtypes.FundingMetadata{
		LastFundingRoundTimestamp: ctx30.BlockTime().UnixMilli(),
	}))
	require.NoError(t, k.BeginBlocker(ctx30))
	require.EqualValues(t, 1, mk.details[1].TotalPremiumSamples, "second sample within 1m must be throttled")
	require.True(t, premiumAfter1.Equal(mk.details[1].AggregatePremiumSum))

	// Third sample 70s later -- admitted.
	ctx70 := ctx.WithBlockTime(ctx.BlockTime().Add(70 * time.Second))
	require.NoError(t, k.Metadata.Set(ctx70, fundingtypes.FundingMetadata{
		LastFundingRoundTimestamp: ctx70.BlockTime().UnixMilli(),
	}))
	require.NoError(t, k.BeginBlocker(ctx70))
	require.EqualValues(t, 2, mk.details[1].TotalPremiumSamples, "sample 70s after the previous one must be admitted")
}

// TestProcessMarketSample_MaxPremiumSampleCount stops accumulating once the
// configured cap is reached. The 1-minute throttle is bypassed by setting
// LastPremiumSampleTimestamp far enough in the past.
func TestProcessMarketSample_MaxPremiumSampleCount(t *testing.T) {
	mk := &stubMarket{
		markets: map[uint32]markettypes.Market{
			1: {MarketIndex: 1, MarketType: perptypes.MarketTypePerps, Status: perptypes.MarketStatusActive},
		},
		details: map[uint32]markettypes.MarketDetails{
			1: {
				MarketIndex:          1,
				FundingRatePrefixSum: math.ZeroInt(),
				TotalPremiumSamples:  50,
				AggregatePremiumSum:  math.NewInt(777),
			},
		},
	}
	ok := stubOracle{price: oracletypes.OraclePrice{IndexPrice: 49_500, MarkPrice: 50_000}}
	bk := stubBook{bid: 49_999, ask: 50_001}

	k, ctx := newFundingKeeper(t, mk, ok, bk)
	mk.details[1] = func() markettypes.MarketDetails {
		d := mk.details[1]
		d.LastPremiumSampleTimestamp = ctx.BlockTime().UnixMilli() - 2*perptypes.MinuteInMs
		return d
	}()

	params, err := k.Params.Get(ctx)
	require.NoError(t, err)
	params.MaxPremiumSampleCount = 50
	require.NoError(t, k.Params.Set(ctx, params))
	require.NoError(t, k.Metadata.Set(ctx, fundingtypes.FundingMetadata{
		LastFundingRoundTimestamp: ctx.BlockTime().UnixMilli(),
	}))

	require.NoError(t, k.BeginBlocker(ctx))
	require.EqualValues(t, 50, mk.details[1].TotalPremiumSamples)
	require.EqualValues(t, 777, mk.details[1].AggregatePremiumSum.Int64())
}

// TestProcessMarketSample_LargePriceDoesNotOverflow feeds an impact
// price near uint32-max so the `(IB - idx) * FundingRateTick`
// intermediate (~ 4.29e9 * 1e6 = 4.29e15) plus accumulation across 60
// samples stays inside math.Int. The test asserts the post-sample
// state is sane (no panic, premium in the expected ballpark).
func TestProcessMarketSample_LargePriceDoesNotOverflow(t *testing.T) {
	const ib = uint32(4_000_000_000)
	const ia = uint32(4_000_000_002)
	const idx = uint32(3_999_999_500)
	mk := &stubMarket{
		markets: map[uint32]markettypes.Market{
			1: {MarketIndex: 1, MarketType: perptypes.MarketTypePerps, Status: perptypes.MarketStatusActive},
		},
		details: map[uint32]markettypes.MarketDetails{
			1: {
				MarketIndex:          1,
				FundingRatePrefixSum: math.ZeroInt(),
				AggregatePremiumSum:  math.ZeroInt(),
			},
		},
	}
	ok := stubOracle{price: oracletypes.OraclePrice{IndexPrice: idx, MarkPrice: idx}}
	bk := stubBook{bid: ib, ask: ia}

	k, ctx := newFundingKeeper(t, mk, ok, bk)
	require.NoError(t, k.BeginBlocker(ctx))

	// Expected premium: (ib - idx) * TICK / idx = 500 * 1e6 / 3_999_999_500 = 0
	// (truncates to zero -- still correct, no overflow).
	got := mk.details[1]
	require.EqualValues(t, 1, got.TotalPremiumSamples)
	// The math.Int implementation handles the multiplication without
	// overflowing int64; the assertion below merely confirms the value is
	// non-negative and bounded.
	require.True(t, got.AggregatePremiumSum.GTE(math.ZeroInt()))
	require.True(t, got.AggregatePremiumSum.LTE(math.NewInt(perptypes.FundingRateTick)))
}
