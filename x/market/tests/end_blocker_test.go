// end_blocker_test.go pins the auto-expiry sweep run by EndBlocker:
// budget enforcement, ApplyExitPosition wiring, nil-keeper fallback,
// error recording and stale-index cleanup. These tests guard the
// invariant that a past ExpiryTimestamp drives every market through
// MarketStatusExpired exactly once.
package tests

import (
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"cosmossdk.io/collections"

	perptypes "github.com/perpdex/perpdex-l1/types"
	"github.com/perpdex/perpdex-l1/x/market/types"
)

func TestEndBlocker_AutoExpiresViaIndex(t *testing.T) {
	env := newTestEnv(t)
	// Two markets: one expired, one still active. ValidateBasic
	// rejects "past expiry" at the msg layer so we go around it by
	// creating the market with a future expiry and then editing the
	// store + ExpiryIndex directly.
	past := env.ctx.BlockTime().Add(-time.Minute).UnixMilli()

	_, err := env.srv.CreateMarket(env.ctx, validCreatePerpMsg(1))
	require.NoError(t, err)
	m, _ := env.keeper.GetMarket(env.ctx, 1)
	m.ExpiryTimestamp = past
	require.NoError(t, env.keeper.Markets.Set(env.ctx, 1, m))
	require.NoError(t, env.keeper.ExpiryIndex.Set(env.ctx, collections.Join(past, uint32(1))))

	msg2 := validCreatePerpMsg(2)
	msg2.Market.ExpiryTimestamp = env.ctx.BlockTime().Add(time.Hour).UnixMilli()
	_, err = env.srv.CreateMarket(env.ctx, msg2)
	require.NoError(t, err)

	require.NoError(t, env.keeper.EndBlocker(env.ctx))

	m1, _ := env.keeper.GetMarket(env.ctx, 1)
	require.Equal(t, perptypes.MarketStatusExpired, m1.Status, "market 1 must be expired")
	m2, _ := env.keeper.GetMarket(env.ctx, 2)
	require.Equal(t, perptypes.MarketStatusActive, m2.Status, "market 2 must stay active")

	require.Contains(t, env.liq.calls, uint32(1))
	require.NotContains(t, env.liq.calls, uint32(2))

	has, _ := env.keeper.ExpiryIndex.Has(env.ctx, collections.Join(past, uint32(1)))
	require.False(t, has)
}

func TestEndBlocker_RespectsBudget(t *testing.T) {
	env := newTestEnv(t)
	past := env.ctx.BlockTime().Add(-time.Minute).UnixMilli()
	for i := uint32(1); i <= 3; i++ {
		msg := validCreatePerpMsg(i)
		_, err := env.srv.CreateMarket(env.ctx, msg)
		require.NoError(t, err)
		m, _ := env.keeper.GetMarket(env.ctx, i)
		m.ExpiryTimestamp = past
		require.NoError(t, env.keeper.Markets.Set(env.ctx, i, m))
		require.NoError(t, env.keeper.ExpiryIndex.Set(env.ctx, collections.Join(past, i)))
	}
	p, _ := env.keeper.Params.Get(env.ctx)
	p.MaxMarketsExpiredPerBlock = 2
	require.NoError(t, env.keeper.Params.Set(env.ctx, p))

	require.NoError(t, env.keeper.EndBlocker(env.ctx))
	require.Len(t, env.liq.calls, 2, "exactly 2 markets must be processed this block")

	// Second pass: remaining 1 market still pending.
	require.NoError(t, env.keeper.EndBlocker(env.ctx))
	require.Len(t, env.liq.calls, 3)
}

func TestEndBlocker_BudgetZeroDisablesAutoExpire(t *testing.T) {
	env := newTestEnv(t)
	past := env.ctx.BlockTime().Add(-time.Minute).UnixMilli()
	_, err := env.srv.CreateMarket(env.ctx, validCreatePerpMsg(1))
	require.NoError(t, err)
	m, _ := env.keeper.GetMarket(env.ctx, 1)
	m.ExpiryTimestamp = past
	require.NoError(t, env.keeper.Markets.Set(env.ctx, 1, m))
	require.NoError(t, env.keeper.ExpiryIndex.Set(env.ctx, collections.Join(past, uint32(1))))

	p, _ := env.keeper.Params.Get(env.ctx)
	p.MaxMarketsExpiredPerBlock = 0
	require.NoError(t, env.keeper.Params.Set(env.ctx, p))

	require.NoError(t, env.keeper.EndBlocker(env.ctx))
	require.Empty(t, env.liq.calls, "budget=0 must disable auto-expiry")
	got, _ := env.keeper.GetMarket(env.ctx, 1)
	require.Equal(t, perptypes.MarketStatusActive, got.Status)
}

func TestEndBlocker_NilLiquidationKeeperEmitsEvent(t *testing.T) {
	env := newTestEnvWithoutLiquidation(t)
	past := env.ctx.BlockTime().Add(-time.Minute).UnixMilli()
	_, err := env.srv.CreateMarket(env.ctx, validCreatePerpMsg(1))
	require.NoError(t, err)
	m, _ := env.keeper.GetMarket(env.ctx, 1)
	m.ExpiryTimestamp = past
	require.NoError(t, env.keeper.Markets.Set(env.ctx, 1, m))
	require.NoError(t, env.keeper.ExpiryIndex.Set(env.ctx, collections.Join(past, uint32(1))))

	require.NoError(t, env.keeper.EndBlocker(env.ctx), "EndBlocker must not panic with nil liquidationKeeper")

	got, _ := env.keeper.GetMarket(env.ctx, 1)
	require.Equal(t, perptypes.MarketStatusExpired, got.Status)

	failedEmitted := false
	for _, ev := range env.ctx.EventManager().Events() {
		if ev.Type == types.EventTypeMarketExpireExitFailed {
			failedEmitted = true
		}
	}
	require.True(t, failedEmitted, "EventTypeMarketExpireExitFailed must be emitted when liquidationKeeper missing")
}

func TestEndBlocker_ApplyExitErrorIsRecorded(t *testing.T) {
	env := newTestEnv(t)
	env.liq.failErr = errors.New("synthetic apply-exit failure")

	past := env.ctx.BlockTime().Add(-time.Minute).UnixMilli()
	_, err := env.srv.CreateMarket(env.ctx, validCreatePerpMsg(1))
	require.NoError(t, err)
	m, _ := env.keeper.GetMarket(env.ctx, 1)
	m.ExpiryTimestamp = past
	require.NoError(t, env.keeper.Markets.Set(env.ctx, 1, m))
	require.NoError(t, env.keeper.ExpiryIndex.Set(env.ctx, collections.Join(past, uint32(1))))

	require.NoError(t, env.keeper.EndBlocker(env.ctx))

	got, _ := env.keeper.GetMarket(env.ctx, 1)
	require.Equal(t, perptypes.MarketStatusExpired, got.Status,
		"market must still be EXPIRED even when ApplyExitPosition errors")
	failedEmitted := false
	for _, ev := range env.ctx.EventManager().Events() {
		if ev.Type == types.EventTypeMarketExpireExitFailed {
			failedEmitted = true
		}
	}
	require.True(t, failedEmitted)
}

func TestEndBlocker_IndexDriftCleanedUp(t *testing.T) {
	env := newTestEnv(t)
	past := env.ctx.BlockTime().Add(-time.Minute).UnixMilli()
	// Put a dangling entry in the index that has no corresponding
	// Market record. EndBlocker must drop it instead of looping.
	require.NoError(t, env.keeper.ExpiryIndex.Set(env.ctx, collections.Join(past, uint32(99))))

	require.NoError(t, env.keeper.EndBlocker(env.ctx))
	has, _ := env.keeper.ExpiryIndex.Has(env.ctx, collections.Join(past, uint32(99)))
	require.False(t, has)
}
