package keeper_test

import (
	"testing"

	"github.com/stretchr/testify/require"

	"cosmossdk.io/math"

	perptypes "github.com/perpdex/perpdex-l1/types"
	fundingtypes "github.com/perpdex/perpdex-l1/x/funding/types"
	markettypes "github.com/perpdex/perpdex-l1/x/market/types"
	oracletypes "github.com/perpdex/perpdex-l1/x/oracle/types"
)

// These tests pin the per-block `refreshMarkPrice` pipeline (see
// `x/funding/keeper/abci.go`) which recomputes
// `MarketDetails.MarkPrice` as
//
//	median(impact_price, price_1, price_2)
//
// where
//
//	price_1 = index + index * (avg_premium / FundingRateTick)
//	price_2 = oracle weighted-median mark.
//
// We drive the pipeline by running a single BeginBlocker pass with a
// pre-seeded MarketDetails snapshot and the orderbook stub returning
// ok=false (so processMarketSample does NOT mutate ImpactPrice in the
// same block — we want to isolate refreshMarkPrice).

// TestRefreshMarkPrice_MedianOfThreeSelectsMiddle pins down the
// happy-path median: with impact, price_1, price_2 spread out, the
// middle value wins.
func TestRefreshMarkPrice_MedianOfThreeSelectsMiddle(t *testing.T) {
	mk := &stubMarket{
		markets: map[uint32]markettypes.Market{
			1: {MarketIndex: 1, MarketType: perptypes.MarketTypePerps, Status: perptypes.MarketStatusActive},
		},
		details: map[uint32]markettypes.MarketDetails{
			1: {
				MarketIndex:          1,
				ImpactPrice:          50_100, // highest
				IndexPrice:           50_000,
				MarkPrice:            0, // not yet written
				AggregatePremiumSum:  math.ZeroInt(),
				TotalPremiumSamples:  0,
				FundingRatePrefixSum: math.ZeroInt(),
			},
		},
	}
	// price_1 = index + index * (0 / TICK) = index = 50_000 (lowest).
	// price_2 = oracle_mark = 50_050 (middle).
	// median(50_100, 50_000, 50_050) = 50_050.
	ok := stubOracle{price: oracletypes.OraclePrice{IndexPrice: 50_000, MarkPrice: 50_050}}
	bk := stubBook{bidOk: false, askOk: false}

	k, ctx := newFundingKeeper(t, mk, ok, bk)
	// Pin LastUpdatedTimestamp = now so processMarketSample's 1-minute
	// throttle drops before it can overwrite ImpactPrice.
	mk.details[1] = func() markettypes.MarketDetails {
		d := mk.details[1]
		d.LastUpdatedTimestamp = ctx.BlockTime().UnixMilli()
		return d
	}()

	require.NoError(t, k.BeginBlocker(ctx))

	got := mk.details[1]
	require.EqualValues(t, 50_050, got.MarkPrice,
		"median of (impact=50_100, price_1=50_000, price_2=50_050) must be 50_050")
	require.EqualValues(t, ctx.BlockTime().UnixMilli(), got.LastMarkPriceTimestamp,
		"refreshMarkPrice must bump LastMarkPriceTimestamp on every successful update")
}

// TestRefreshMarkPrice_ImpactZeroFallsBackToMean verifies the
// fallback path when one side of the book has drained: ImpactPrice =
// 0 forces the medianer to mean(price_1, price_2).
func TestRefreshMarkPrice_ImpactZeroFallsBackToMean(t *testing.T) {
	mk := &stubMarket{
		markets: map[uint32]markettypes.Market{
			1: {MarketIndex: 1, MarketType: perptypes.MarketTypePerps, Status: perptypes.MarketStatusActive},
		},
		details: map[uint32]markettypes.MarketDetails{
			1: {
				MarketIndex:          1,
				ImpactPrice:          0, // both sides drained
				IndexPrice:           50_000,
				MarkPrice:            0,
				AggregatePremiumSum:  math.ZeroInt(),
				TotalPremiumSamples:  0,
				FundingRatePrefixSum: math.ZeroInt(),
			},
		},
	}
	// price_1 = 50_000, price_2 = 50_100; mean = 50_050.
	ok := stubOracle{price: oracletypes.OraclePrice{IndexPrice: 50_000, MarkPrice: 50_100}}
	bk := stubBook{bidOk: false, askOk: false}

	k, ctx := newFundingKeeper(t, mk, ok, bk)
	mk.details[1] = func() markettypes.MarketDetails {
		d := mk.details[1]
		d.LastUpdatedTimestamp = ctx.BlockTime().UnixMilli()
		return d
	}()

	require.NoError(t, k.BeginBlocker(ctx))
	require.EqualValues(t, 50_050, mk.details[1].MarkPrice,
		"impact=0 must fall back to mean(price_1, price_2) = (50_000 + 50_100)/2")
}

// TestRefreshMarkPrice_PremiumShiftsPrice1 verifies that a non-zero
// AggregatePremiumSum pushes price_1 away from the index toward the
// premium-implied mid.
//
// AggregatePremiumSum = 10_000_000 (avg = 10_000_000 / 1 = 10_000_000).
// price_1 = 50_000 + 50_000 * (10_000_000 / 1_000_000) = 50_000 + 500_000.
// That's huge — clamp into the median: median(impact=50_000,
// price_1=550_000, price_2=50_500) = 50_500.
func TestRefreshMarkPrice_PremiumShiftsPrice1(t *testing.T) {
	mk := &stubMarket{
		markets: map[uint32]markettypes.Market{
			1: {MarketIndex: 1, MarketType: perptypes.MarketTypePerps, Status: perptypes.MarketStatusActive},
		},
		details: map[uint32]markettypes.MarketDetails{
			1: {
				MarketIndex:          1,
				ImpactPrice:          50_000,
				IndexPrice:           50_000,
				MarkPrice:            0,
				AggregatePremiumSum:  math.NewInt(10_000_000),
				TotalPremiumSamples:  1,
				FundingRatePrefixSum: math.ZeroInt(),
			},
		},
	}
	ok := stubOracle{price: oracletypes.OraclePrice{IndexPrice: 50_000, MarkPrice: 50_500}}
	bk := stubBook{bidOk: false, askOk: false}

	k, ctx := newFundingKeeper(t, mk, ok, bk)
	mk.details[1] = func() markettypes.MarketDetails {
		d := mk.details[1]
		d.LastUpdatedTimestamp = ctx.BlockTime().UnixMilli()
		return d
	}()

	require.NoError(t, k.BeginBlocker(ctx))
	require.EqualValues(t, 50_500, mk.details[1].MarkPrice,
		"premium tilts price_1 high; median selects oracle mark (50_500)")
}

// TestRefreshMarkPrice_OracleStalePreservesLastGoodMark verifies the
// "do not zero on transient oracle failure" path: a stale oracle
// causes refreshMarkPrice to return early without mutating
// d.MarkPrice. Downstream readers (x/risk) gate on
// LastMarkPriceTimestamp + MaxMarkStalenessMs, so a brief outage
// keeps the last good mark in place while a long outage eventually
// trips the gate.
func TestRefreshMarkPrice_OracleStalePreservesLastGoodMark(t *testing.T) {
	mk := &stubMarket{
		markets: map[uint32]markettypes.Market{
			1: {MarketIndex: 1, MarketType: perptypes.MarketTypePerps, Status: perptypes.MarketStatusActive},
		},
		details: map[uint32]markettypes.MarketDetails{
			1: {
				MarketIndex:            1,
				ImpactPrice:            50_000,
				IndexPrice:             49_900,
				MarkPrice:              49_950,            // last good
				LastMarkPriceTimestamp: 1_700_000_000_000, // arbitrary fresh
				AggregatePremiumSum:    math.ZeroInt(),
				FundingRatePrefixSum:   math.ZeroInt(),
			},
		},
	}
	ok := stubOracle{err: oracletypes.ErrStalePrice}
	bk := stubBook{bidOk: false, askOk: false}

	k, ctx := newFundingKeeper(t, mk, ok, bk)
	mk.details[1] = func() markettypes.MarketDetails {
		d := mk.details[1]
		d.LastUpdatedTimestamp = ctx.BlockTime().UnixMilli()
		return d
	}()
	preDetails := mk.details[1]

	require.NoError(t, k.BeginBlocker(ctx))
	got := mk.details[1]
	require.EqualValues(t, preDetails.MarkPrice, got.MarkPrice,
		"oracle stale must preserve d.MarkPrice (last good)")
	require.EqualValues(t, preDetails.LastMarkPriceTimestamp, got.LastMarkPriceTimestamp,
		"oracle stale must NOT bump LastMarkPriceTimestamp (so staleness gate can eventually trip)")
}

// TestRefreshMarkPrice_NoSamplesPrice1EqualsIndex pins the edge case
// where the funding window has produced no samples yet (fresh market
// / start of new round). avg_premium is treated as 0, so
// price_1 = index_price (no premium component), and the median
// reduces to median(impact, index, oracle_mark).
func TestRefreshMarkPrice_NoSamplesPrice1EqualsIndex(t *testing.T) {
	mk := &stubMarket{
		markets: map[uint32]markettypes.Market{
			1: {MarketIndex: 1, MarketType: perptypes.MarketTypePerps, Status: perptypes.MarketStatusActive},
		},
		details: map[uint32]markettypes.MarketDetails{
			1: {
				MarketIndex:          1,
				ImpactPrice:          50_100,
				IndexPrice:           50_000,
				MarkPrice:            0,
				AggregatePremiumSum:  math.ZeroInt(),
				TotalPremiumSamples:  0, // no samples yet
				FundingRatePrefixSum: math.ZeroInt(),
			},
		},
	}
	ok := stubOracle{price: oracletypes.OraclePrice{IndexPrice: 50_000, MarkPrice: 50_050}}
	bk := stubBook{bidOk: false, askOk: false}

	k, ctx := newFundingKeeper(t, mk, ok, bk)
	mk.details[1] = func() markettypes.MarketDetails {
		d := mk.details[1]
		d.LastUpdatedTimestamp = ctx.BlockTime().UnixMilli()
		return d
	}()

	require.NoError(t, k.BeginBlocker(ctx))
	// median(impact=50_100, price_1=50_000, price_2=50_050) = 50_050.
	require.EqualValues(t, 50_050, mk.details[1].MarkPrice)
}

// TestMaxMarkStalenessMs_DefaultedParamsExposeSetting verifies the
// `FundingKeeper.MaxMarkStalenessMs` accessor returns the value
// stored in params. This is the integration point consumed by x/risk
// to gate stale mark reads.
func TestMaxMarkStalenessMs_DefaultedParamsExposeSetting(t *testing.T) {
	mk := &stubMarket{}
	ok := stubOracle{}
	bk := stubBook{}
	k, ctx := newFundingKeeper(t, mk, ok, bk)

	got, err := k.MaxMarkStalenessMs(ctx)
	require.NoError(t, err)
	require.EqualValues(t, fundingtypes.DefaultMaxMarkStalenessMs, got)

	p, err := k.Params.Get(ctx)
	require.NoError(t, err)
	p.MaxMarkStalenessMs = 12_345
	require.NoError(t, k.Params.Set(ctx, p))
	got, err = k.MaxMarkStalenessMs(ctx)
	require.NoError(t, err)
	require.EqualValues(t, 12_345, got)
}
