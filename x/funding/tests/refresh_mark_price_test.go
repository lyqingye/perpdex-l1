// refresh_mark_price_test.go pins the per-block `refreshMarkPrice`
// pipeline in `x/funding/keeper/abci.go` which recomputes
// `MarketDetails.MarkPrice` as:
//
//	premium_raw = clamp(impact_price - index, ±index/MarkPremiumClampDivisor)
//	premium_ema = ema_step(prev_ema, premium_raw, dt, MarkPremiumEmaTauMs)
//	price_1     = clampUint32(index + premium_ema)
//	price_2     = oracle weighted-median mark
//	mark        = median3(impact_price, price_1, price_2)
//
// The orderbook stub returns ok=false so `processMarketSample` never
// overwrites d.ImpactPrice within the tested block (we want to isolate
// refreshMarkPrice).
package tests

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"cosmossdk.io/math"

	perptypes "github.com/perpdex/perpdex-l1/types"
	markettypes "github.com/perpdex/perpdex-l1/x/market/types"
	oracletypes "github.com/perpdex/perpdex-l1/x/oracle/types"
)

// TestRefreshMarkPrice_MedianOfThreeSelectsMiddle verifies the
// happy-path median: with impact, price_1, price_2 spread out, the
// middle value wins. We start the EMA at a small `prev` and pin
// `LastMarkPriceRefreshTimestamp = now - 1ms` so the step from `raw` to
// `prev + delta` is bounded by floor((raw-prev)*1/TAU) = 0 and price_1
// stays near `index + prev_ema` — well below the impact and oracle mid.
func TestRefreshMarkPrice_MedianOfThreeSelectsMiddle(t *testing.T) {
	mk := &stubMarket{
		markets: map[uint32]markettypes.Market{
			1: {MarketIndex: 1, MarketType: perptypes.MarketTypePerps, Status: perptypes.MarketStatusActive},
		},
		details: map[uint32]markettypes.MarketDetails{
			1: {
				MarketIndex:          1,
				ImpactPrice:          50_200, // highest input
				IndexPrice:           50_000,
				MarkPrice:            0,
				MarkPremiumEma:       0,
				AggregatePremiumSum:  math.ZeroInt(),
				FundingRatePrefixSum: math.ZeroInt(),
			},
		},
	}
	ok := stubOracle{price: oracletypes.OraclePrice{IndexPrice: 50_000, MarkPrice: 50_100}}
	bk := stubBook{}

	k, ctx := newFundingKeeper(t, mk, ok, bk)
	// dt=1ms keeps the EMA step (raw-prev)*dt/TAU < 1, so price_1 ≈ idx.
	// Also pin LastPremiumSampleTimestamp = now so processMarketSample is
	// throttled out.
	mk.details[1] = func() markettypes.MarketDetails {
		d := mk.details[1]
		d.LastPremiumSampleTimestamp = ctx.BlockTime().UnixMilli()
		d.LastMarkPriceRefreshTimestamp = ctx.BlockTime().UnixMilli() - 1
		return d
	}()

	require.NoError(t, k.BeginBlocker(ctx))

	got := mk.details[1]
	// EMA step is floor((200-0)*1/480_000)=0, so price_1 = idx = 50_000.
	// median(impact=50_200, price_1=50_000, price_2=50_100) = 50_100.
	require.EqualValues(t, 50_100, got.MarkPrice)
	require.EqualValues(t, 0, got.MarkPremiumEma,
		"EMA step under-flows to zero with dt=1ms and TAU=8m")
	require.EqualValues(t, ctx.BlockTime().UnixMilli(), got.LastMarkPriceRefreshTimestamp)
}

// TestRefreshMarkPrice_FirstCallReseedsEma pins the bootstrap
// behaviour: when `LastMarkPriceRefreshTimestamp == 0` the EMA is reseeded to
// `raw` instead of being slowly integrated from 0.
func TestRefreshMarkPrice_FirstCallReseedsEma(t *testing.T) {
	mk := &stubMarket{
		markets: map[uint32]markettypes.Market{
			1: {MarketIndex: 1, MarketType: perptypes.MarketTypePerps, Status: perptypes.MarketStatusActive},
		},
		details: map[uint32]markettypes.MarketDetails{
			1: {
				MarketIndex:                   1,
				ImpactPrice:                   50_100, // +100 above index
				IndexPrice:                    50_000,
				MarkPrice:                     0,
				MarkPremiumEma:                0,
				LastMarkPriceRefreshTimestamp: 0,
				AggregatePremiumSum:           math.ZeroInt(),
				FundingRatePrefixSum:          math.ZeroInt(),
			},
		},
	}
	// oracle_mark deliberately placed BELOW index so the three median
	// inputs (impact=50_100, price_1=idx+ema, oracle_mark=49_900) are
	// distinct. This makes the MarkPrice assertion fail if the EMA is
	// reseeded to anything other than +100 (e.g. 0 or a half-step
	// would push price_1 below impact and flip the median ordering).
	ok := stubOracle{price: oracletypes.OraclePrice{IndexPrice: 50_000, MarkPrice: 49_900}}
	bk := stubBook{}

	k, ctx := newFundingKeeper(t, mk, ok, bk)
	mk.details[1] = func() markettypes.MarketDetails {
		d := mk.details[1]
		d.LastPremiumSampleTimestamp = ctx.BlockTime().UnixMilli()
		return d
	}()

	require.NoError(t, k.BeginBlocker(ctx))

	got := mk.details[1]
	// raw = clamp(100, ±50_000/200=250) = 100; first call reseeds EMA = 100.
	// price_1 = 50_000 + 100 = 50_100.
	// median(impact=50_100, price_1=50_100, oracle_mark=49_900) = 50_100.
	require.EqualValues(t, 100, got.MarkPremiumEma,
		"first refresh must reseed EMA = raw_premium")
	require.EqualValues(t, 50_100, got.MarkPrice)
}

// TestRefreshMarkPrice_ClampLimitsPremium verifies the ±index/200
// (±0.5%) clamp: an impact_price far above index cannot push price_1
// arbitrarily high.
func TestRefreshMarkPrice_ClampLimitsPremium(t *testing.T) {
	mk := &stubMarket{
		markets: map[uint32]markettypes.Market{
			1: {MarketIndex: 1, MarketType: perptypes.MarketTypePerps, Status: perptypes.MarketStatusActive},
		},
		details: map[uint32]markettypes.MarketDetails{
			1: {
				MarketIndex:          1,
				ImpactPrice:          80_000, // wildly above index
				IndexPrice:           50_000,
				MarkPrice:            0,
				MarkPremiumEma:       0,
				AggregatePremiumSum:  math.ZeroInt(),
				FundingRatePrefixSum: math.ZeroInt(),
			},
		},
	}
	ok := stubOracle{price: oracletypes.OraclePrice{IndexPrice: 50_000, MarkPrice: 50_000}}
	bk := stubBook{}

	k, ctx := newFundingKeeper(t, mk, ok, bk)
	mk.details[1] = func() markettypes.MarketDetails {
		d := mk.details[1]
		d.LastPremiumSampleTimestamp = ctx.BlockTime().UnixMilli()
		return d
	}()

	require.NoError(t, k.BeginBlocker(ctx))

	got := mk.details[1]
	// raw = clamp(30_000, ±250) = 250. first call → EMA = 250.
	// price_1 = 50_000 + 250 = 50_250.
	// median(impact=80_000, price_1=50_250, oracle_mark=50_000) = 50_250.
	require.EqualValues(t, 250, got.MarkPremiumEma,
		"premium must clamp at index / MarkPremiumClampDivisor (50_000/200=250)")
	require.EqualValues(t, 50_250, got.MarkPrice,
		"clamped price_1 must enter the median; impact spike does not drag mark up")
}

// TestRefreshMarkPrice_ImpactZeroFallsBackToMean verifies the fallback
// when one side of the book has drained: ImpactPrice = 0 freezes
// premium_raw to 0 and forces the median to mean(price_1, price_2).
func TestRefreshMarkPrice_ImpactZeroFallsBackToMean(t *testing.T) {
	mk := &stubMarket{
		markets: map[uint32]markettypes.Market{
			1: {MarketIndex: 1, MarketType: perptypes.MarketTypePerps, Status: perptypes.MarketStatusActive},
		},
		details: map[uint32]markettypes.MarketDetails{
			1: {
				MarketIndex:          1,
				ImpactPrice:          0,
				IndexPrice:           50_000,
				MarkPrice:            0,
				MarkPremiumEma:       0,
				AggregatePremiumSum:  math.ZeroInt(),
				FundingRatePrefixSum: math.ZeroInt(),
			},
		},
	}
	ok := stubOracle{price: oracletypes.OraclePrice{IndexPrice: 50_000, MarkPrice: 50_100}}
	bk := stubBook{}

	k, ctx := newFundingKeeper(t, mk, ok, bk)
	mk.details[1] = func() markettypes.MarketDetails {
		d := mk.details[1]
		d.LastPremiumSampleTimestamp = ctx.BlockTime().UnixMilli()
		return d
	}()

	require.NoError(t, k.BeginBlocker(ctx))
	got := mk.details[1]
	// raw=0 (impact==0 skip), first call EMA=0, price_1=50_000.
	// median path: impact=0 ⇒ mean(price_1, price_2) = mean(50_000, 50_100) = 50_050.
	require.EqualValues(t, 50_050, got.MarkPrice)
	require.EqualValues(t, 0, got.MarkPremiumEma)
}

// TestRefreshMarkPrice_LongGapResetsEma verifies that a gap >= TAU
// reseeds the EMA from `raw` (rather than slow-integrating the old
// value forward). This prevents stale momentum from carrying over
// after a long funding outage.
func TestRefreshMarkPrice_LongGapResetsEma(t *testing.T) {
	now := int64(1_700_000_000_000)
	mk := &stubMarket{
		markets: map[uint32]markettypes.Market{
			1: {MarketIndex: 1, MarketType: perptypes.MarketTypePerps, Status: perptypes.MarketStatusActive},
		},
		details: map[uint32]markettypes.MarketDetails{
			1: {
				MarketIndex:                   1,
				ImpactPrice:                   50_050,
				IndexPrice:                    50_000,
				MarkPrice:                     0,
				MarkPremiumEma:                200, // stale momentum
				LastMarkPriceRefreshTimestamp: now - perptypes.MarkPremiumEmaTauMs - 1,
				AggregatePremiumSum:           math.ZeroInt(),
				FundingRatePrefixSum:          math.ZeroInt(),
			},
		},
	}
	ok := stubOracle{price: oracletypes.OraclePrice{IndexPrice: 50_000, MarkPrice: 50_025}}
	bk := stubBook{}

	k, ctx := newFundingKeeper(t, mk, ok, bk)
	mk.details[1] = func() markettypes.MarketDetails {
		d := mk.details[1]
		d.LastPremiumSampleTimestamp = ctx.BlockTime().UnixMilli()
		return d
	}()

	require.NoError(t, k.BeginBlocker(ctx))

	got := mk.details[1]
	// dt >= TAU triggers reset: EMA = raw = clamp(50, ±250) = 50.
	require.EqualValues(t, 50, got.MarkPremiumEma,
		"long gap must reseed EMA to raw, discarding stale momentum")
	// price_1 = 50_050, median(impact=50_050, price_1=50_050, price_2=50_025) = 50_050.
	require.EqualValues(t, 50_050, got.MarkPrice)
}

// TestRefreshMarkPrice_OracleStalePreservesLastGoodMark verifies the
// "do not zero on transient oracle failure" path: a stale oracle
// causes refreshMarkPrice to return early without mutating
// d.MarkPrice / d.MarkPremiumEma. Downstream readers gate on
// LastMarkPriceRefreshTimestamp + MaxMarkPriceStalenessMs (now in market.Params),
// so a brief outage keeps the last good mark in place while a long
// outage eventually trips the gate.
func TestRefreshMarkPrice_OracleStalePreservesLastGoodMark(t *testing.T) {
	mk := &stubMarket{
		markets: map[uint32]markettypes.Market{
			1: {MarketIndex: 1, MarketType: perptypes.MarketTypePerps, Status: perptypes.MarketStatusActive},
		},
		details: map[uint32]markettypes.MarketDetails{
			1: {
				MarketIndex:                   1,
				ImpactPrice:                   50_000,
				IndexPrice:                    49_900,
				MarkPrice:                     49_950,
				MarkPremiumEma:                77,
				LastMarkPriceRefreshTimestamp: 1_700_000_000_000,
				AggregatePremiumSum:           math.ZeroInt(),
				FundingRatePrefixSum:          math.ZeroInt(),
			},
		},
	}
	ok := stubOracle{err: oracletypes.ErrStalePrice}
	bk := stubBook{}

	k, ctx := newFundingKeeper(t, mk, ok, bk)
	mk.details[1] = func() markettypes.MarketDetails {
		d := mk.details[1]
		d.LastPremiumSampleTimestamp = ctx.BlockTime().UnixMilli()
		return d
	}()
	pre := mk.details[1]

	require.NoError(t, k.BeginBlocker(ctx))
	got := mk.details[1]
	require.EqualValues(t, pre.MarkPrice, got.MarkPrice,
		"oracle stale must preserve d.MarkPrice (last good)")
	require.EqualValues(t, pre.MarkPremiumEma, got.MarkPremiumEma,
		"oracle stale must preserve EMA state")
	require.EqualValues(t, pre.LastMarkPriceRefreshTimestamp, got.LastMarkPriceRefreshTimestamp,
		"oracle stale must NOT bump LastMarkPriceRefreshTimestamp")
}

// TestRefreshMarkPrice_EmaConvergesUnderRepeatedRaw drives BeginBlocker
// across multiple blocks while advancing BlockTime so `dt > 0` and the
// EMA step actually fires. After the first block reseeds EMA to raw,
// subsequent blocks with raw == prev_ema produce `(raw-prev)*dt/tau=0`
// and EMA stays put. We then flip raw and verify the EMA drifts toward
// the new target monotonically without overshooting.
func TestRefreshMarkPrice_EmaConvergesUnderRepeatedRaw(t *testing.T) {
	mk := &stubMarket{
		markets: map[uint32]markettypes.Market{
			1: {MarketIndex: 1, MarketType: perptypes.MarketTypePerps, Status: perptypes.MarketStatusActive},
		},
		details: map[uint32]markettypes.MarketDetails{
			1: {
				MarketIndex:          1,
				ImpactPrice:          50_100, // raw = +100 every block
				IndexPrice:           50_000,
				MarkPrice:            0,
				MarkPremiumEma:       0,
				AggregatePremiumSum:  math.ZeroInt(),
				FundingRatePrefixSum: math.ZeroInt(),
			},
		},
	}
	ok := stubOracle{price: oracletypes.OraclePrice{IndexPrice: 50_000, MarkPrice: 50_100}}
	bk := stubBook{}

	k, ctx := newFundingKeeper(t, mk, ok, bk)
	mk.details[1] = func() markettypes.MarketDetails {
		d := mk.details[1]
		d.LastPremiumSampleTimestamp = ctx.BlockTime().UnixMilli()
		return d
	}()

	// First block reseeds EMA to raw=100 (LastMarkPriceRefreshTimestamp was 0).
	require.NoError(t, k.BeginBlocker(ctx))
	require.EqualValues(t, 100, mk.details[1].MarkPremiumEma)

	// raw==prev under any non-zero dt → delta=0 → EMA holds at 100.
	// We advance LastPremiumSampleTimestamp in lockstep with BlockTime so
	// `processMarketSample` stays throttled (otherwise crossing the
	// 60-second `premiumSampleIntervalMs` boundary would un-throttle
	// it, and with `stubBook{ok:false}` it would clobber d.ImpactPrice
	// = 0 mid-test, silently changing the test's meaning).
	for i := 0; i < 5; i++ {
		ctx = ctx.WithBlockTime(ctx.BlockTime().Add(5 * time.Second))
		mk.details[1] = func() markettypes.MarketDetails {
			d := mk.details[1]
			d.LastPremiumSampleTimestamp = ctx.BlockTime().UnixMilli()
			return d
		}()
		require.NoError(t, k.BeginBlocker(ctx))
	}
	require.EqualValues(t, 100, mk.details[1].MarkPremiumEma,
		"EMA must remain at raw once converged; no drift from repeated steps")
	require.NotZero(t, mk.details[1].ImpactPrice,
		"processMarketSample throttle leaked; test setup invalid")

	// Flip raw to a NEGATIVE target. With dt=5s, tau=8min:
	// step_size ≈ |raw - prev| * dt / tau = 200 * 5000 / 480000 ≈ 2
	// per block (Quo truncates toward zero). EMA should decrease
	// monotonically and never cross raw.
	mk.details[1] = func() markettypes.MarketDetails {
		d := mk.details[1]
		d.ImpactPrice = 49_900 // raw_new = -100
		return d
	}()
	prev := mk.details[1].MarkPremiumEma
	for i := 0; i < 5; i++ {
		ctx = ctx.WithBlockTime(ctx.BlockTime().Add(5 * time.Second))
		mk.details[1] = func() markettypes.MarketDetails {
			d := mk.details[1]
			d.LastPremiumSampleTimestamp = ctx.BlockTime().UnixMilli()
			return d
		}()
		require.NoError(t, k.BeginBlocker(ctx))
		curr := mk.details[1].MarkPremiumEma
		require.Less(t, curr, prev,
			"EMA must drift strictly downward toward negative raw (block %d): prev=%d curr=%d",
			i, prev, curr)
		require.GreaterOrEqual(t, curr, int64(-100),
			"EMA must not overshoot the negative target")
		prev = curr
	}
}
