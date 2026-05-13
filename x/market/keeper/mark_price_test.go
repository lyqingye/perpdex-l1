package keeper_test

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"cosmossdk.io/math"

	"github.com/perpdex/perpdex-l1/x/market/types"
)

// TestGetMarkPrice_Happy verifies the green-path read: a non-zero
// markPrice refreshed within MaxMarkPriceStalenessMs is returned verbatim.
func TestGetMarkPrice_Happy(t *testing.T) {
	env := newTestEnv(t)
	now := env.ctx.BlockTime().UnixMilli()
	require.NoError(t, env.keeper.SetMarketDetails(env.ctx, types.MarketDetails{
		MarketIndex:                   1,
		MarkPrice:                     50_000,
		LastMarkPriceRefreshTimestamp: now,
		FundingRatePrefixSum:          math.ZeroInt(),
		AggregatePremiumSum:           math.ZeroInt(),
	}))

	markPrice, err := env.keeper.GetMarkPrice(env.ctx, 1)
	require.NoError(t, err)
	require.EqualValues(t, 50_000, markPrice)
}

// TestGetMarkPriceAndDetails_Happy verifies the dual return: gated markPrice
// AND the underlying MarketDetails row in a single round-trip.
func TestGetMarkPriceAndDetails_Happy(t *testing.T) {
	env := newTestEnv(t)
	now := env.ctx.BlockTime().UnixMilli()
	require.NoError(t, env.keeper.SetMarketDetails(env.ctx, types.MarketDetails{
		MarketIndex:                   1,
		MarkPrice:                     50_000,
		LastMarkPriceRefreshTimestamp: now,
		DefaultInitialMarginFraction:  100,
		FundingRatePrefixSum:          math.ZeroInt(),
		AggregatePremiumSum:           math.ZeroInt(),
	}))

	markPrice, md, err := env.keeper.GetMarkPriceAndDetails(env.ctx, 1)
	require.NoError(t, err)
	require.EqualValues(t, 50_000, markPrice)
	require.EqualValues(t, 100, md.DefaultInitialMarginFraction)
	require.EqualValues(t, now, md.LastMarkPriceRefreshTimestamp)
}

// TestGetMarkPrice_ZeroFailsClosed enforces the zero-markPrice gate.
// A zero markPrice would silently zero out IM/MM/CM/uPnL; the reader
// MUST surface ErrZeroMarkPrice instead.
func TestGetMarkPrice_ZeroFailsClosed(t *testing.T) {
	env := newTestEnv(t)
	now := env.ctx.BlockTime().UnixMilli()
	require.NoError(t, env.keeper.SetMarketDetails(env.ctx, types.MarketDetails{
		MarketIndex:                   1,
		MarkPrice:                     0, // zero - must trip the gate
		LastMarkPriceRefreshTimestamp: now,
		FundingRatePrefixSum:          math.ZeroInt(),
		AggregatePremiumSum:           math.ZeroInt(),
	}))

	_, err := env.keeper.GetMarkPrice(env.ctx, 1)
	require.ErrorIs(t, err, types.ErrZeroMarkPrice)
}

// TestGetMarkPrice_StaleFailsClosed enforces the staleness gate.
// A non-zero markPrice older than MaxMarkPriceStalenessMs MUST surface
// ErrStaleMarkPrice instead of being treated as live.
func TestGetMarkPrice_StaleFailsClosed(t *testing.T) {
	env := newTestEnv(t)
	now := env.ctx.BlockTime().UnixMilli()
	// Mark "refreshed" an hour ago, well past the 5-minute default
	// MaxMarkPriceStalenessMs.
	require.NoError(t, env.keeper.SetMarketDetails(env.ctx, types.MarketDetails{
		MarketIndex:                   1,
		MarkPrice:                     50_000,
		LastMarkPriceRefreshTimestamp: now - (time.Hour).Milliseconds(),
		FundingRatePrefixSum:          math.ZeroInt(),
		AggregatePremiumSum:           math.ZeroInt(),
	}))

	_, err := env.keeper.GetMarkPrice(env.ctx, 1)
	require.ErrorIs(t, err, types.ErrStaleMarkPrice)
}

// TestGetMarkPrice_NeverRefreshedFailsClosed pins the
// "first-block-after-CreateMarket" path: a fresh market has
// LastMarkPriceRefreshTimestamp = 0 until the funding BeginBlocker writes
// the first markPrice. Reads in that window MUST fail closed.
func TestGetMarkPrice_NeverRefreshedFailsClosed(t *testing.T) {
	env := newTestEnv(t)
	require.NoError(t, env.keeper.SetMarketDetails(env.ctx, types.MarketDetails{
		MarketIndex:                   1,
		MarkPrice:                     50_000,
		LastMarkPriceRefreshTimestamp: 0,
		FundingRatePrefixSum:          math.ZeroInt(),
		AggregatePremiumSum:           math.ZeroInt(),
	}))

	_, err := env.keeper.GetMarkPrice(env.ctx, 1)
	require.ErrorIs(t, err, types.ErrStaleMarkPrice)
}

// TestGetMarkPrice_ZeroStalenessDisablesGate verifies the test /
// genesis-bootstrap escape hatch: setting `MaxMarkPriceStalenessMs = 0`
// turns the staleness check into a no-op. The zero-markPrice gate still
// fires regardless.
func TestGetMarkPrice_ZeroStalenessDisablesGate(t *testing.T) {
	env := newTestEnv(t)
	params, err := env.keeper.Params.Get(env.ctx)
	require.NoError(t, err)
	params.MaxMarkPriceStalenessMs = 0
	require.NoError(t, env.keeper.Params.Set(env.ctx, params))

	require.NoError(t, env.keeper.SetMarketDetails(env.ctx, types.MarketDetails{
		MarketIndex:                   1,
		MarkPrice:                     50_000,
		LastMarkPriceRefreshTimestamp: 0, // would normally trip the gate
		FundingRatePrefixSum:          math.ZeroInt(),
		AggregatePremiumSum:           math.ZeroInt(),
	}))

	markPrice, err := env.keeper.GetMarkPrice(env.ctx, 1)
	require.NoError(t, err)
	require.EqualValues(t, 50_000, markPrice)
}

// TestGetMarkPrice_MissingMarketSurfacesMissingPrice ensures the
// reader maps "market_details row absent" to ErrMissingPrice rather
// than leaking the underlying ErrMarketNotFound (which callers might
// mistake for "market governance pending").
func TestGetMarkPrice_MissingMarketSurfacesMissingPrice(t *testing.T) {
	env := newTestEnv(t)
	_, err := env.keeper.GetMarkPrice(env.ctx, 999)
	require.ErrorIs(t, err, types.ErrMissingPrice)
}

// TestGetMarkPrice_FutureTimestampTreatedFresh pins the clock
// regression / NTP drift edge case: when `LastMarkPriceRefreshTimestamp >
// now` (e.g. after a chain replay against shifted block time), the
// gate must NOT report stale — `now - last` is negative and
// `negative > MaxMarkPriceStalenessMs` is false, so the markPrice is accepted.
// "Newer than now" can never be staler than the bound by definition.
func TestGetMarkPrice_FutureTimestampTreatedFresh(t *testing.T) {
	env := newTestEnv(t)
	now := env.ctx.BlockTime().UnixMilli()
	require.NoError(t, env.keeper.SetMarketDetails(env.ctx, types.MarketDetails{
		MarketIndex:                   1,
		MarkPrice:                     50_000,
		LastMarkPriceRefreshTimestamp: now + (time.Hour).Milliseconds(), // 1h in the future
		FundingRatePrefixSum:          math.ZeroInt(),
		AggregatePremiumSum:           math.ZeroInt(),
	}))

	markPrice, err := env.keeper.GetMarkPrice(env.ctx, 1)
	require.NoError(t, err, "future timestamp must be treated as fresh, not stale")
	require.EqualValues(t, 50_000, markPrice)
}
