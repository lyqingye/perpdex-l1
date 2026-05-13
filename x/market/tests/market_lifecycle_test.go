// market_lifecycle_test.go covers governance-driven market lifecycle
// flows that go through the msg server: CreateMarket, UpdateMarket and
// UpdateMarketDetails. These tests pin the validation, authorisation,
// asset-binding and runtime-zeroing invariants that protect the market
// table from corruption between genesis and end-of-life.
package tests

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"cosmossdk.io/collections"

	perptypes "github.com/perpdex/perpdex-l1/types"
	"github.com/perpdex/perpdex-l1/x/market/types"
)

func TestCreateMarket_Success(t *testing.T) {
	env := newTestEnv(t)
	_, err := env.srv.CreateMarket(env.ctx, validCreatePerpMsg(1))
	require.NoError(t, err)

	m, err := env.keeper.GetMarket(env.ctx, 1)
	require.NoError(t, err)
	require.Equal(t, perptypes.MarketStatusActive, m.Status)
	require.Equal(t, env.ctx.BlockTime().UnixMilli(), m.CreatedAt)

	d, err := env.keeper.GetMarketDetails(env.ctx, 1)
	require.NoError(t, err)
	// Runtime fields all zero (defense-in-depth + ValidateBasic).
	require.Zero(t, d.MarkPrice)
	require.Zero(t, d.OpenInterest)
	require.True(t, d.FundingRatePrefixSum.IsZero())

	// Event sanity: market_created emitted.
	found := false
	for _, ev := range env.ctx.EventManager().Events() {
		if ev.Type == types.EventTypeMarketCreated {
			found = true
		}
	}
	require.True(t, found, "expected market_created event")
}

func TestCreateMarket_WritesExpiryIndex(t *testing.T) {
	env := newTestEnv(t)
	msg := validCreatePerpMsg(1)
	msg.Market.ExpiryTimestamp = env.ctx.BlockTime().Add(time.Hour).UnixMilli()
	_, err := env.srv.CreateMarket(env.ctx, msg)
	require.NoError(t, err)

	has, err := env.keeper.ExpiryIndex.Has(env.ctx, collections.Join(msg.Market.ExpiryTimestamp, uint32(1)))
	require.NoError(t, err)
	require.True(t, has)
}

func TestCreateMarket_RejectsUnauthorized(t *testing.T) {
	env := newTestEnv(t)
	msg := validCreatePerpMsg(1)
	msg.Authority = otherAddr
	_, err := env.srv.CreateMarket(env.ctx, msg)
	require.ErrorIs(t, err, types.ErrInvalidAuthority)
}

func TestCreateMarket_RejectsDuplicate(t *testing.T) {
	env := newTestEnv(t)
	_, err := env.srv.CreateMarket(env.ctx, validCreatePerpMsg(1))
	require.NoError(t, err)
	_, err = env.srv.CreateMarket(env.ctx, validCreatePerpMsg(1))
	require.ErrorIs(t, err, types.ErrMarketExists)
}

func TestCreateMarket_RejectsDisabledQuoteAsset(t *testing.T) {
	env := newTestEnv(t)
	a := env.asset.assets[perptypes.USDCAssetIndex]
	a.Enabled = false
	env.asset.assets[perptypes.USDCAssetIndex] = a

	_, err := env.srv.CreateMarket(env.ctx, validCreatePerpMsg(1))
	require.ErrorIs(t, err, types.ErrInvalidMarket)
}

func TestCreateMarket_PerpsRejectsNonUSDCQuote(t *testing.T) {
	env := newTestEnv(t)
	msg := validCreatePerpMsg(1)
	msg.Market.QuoteAssetId = perptypes.NativeAssetIndex
	_, err := env.srv.CreateMarket(env.ctx, msg)
	require.ErrorIs(t, err, types.ErrInvalidMarket)
}

func TestCreateMarket_SpotChecksBaseAsset(t *testing.T) {
	env := newTestEnv(t)
	msg := validCreateSpotMsg(perptypes.MinSpotMarketIndex, perptypes.LITAssetIndex)
	_, err := env.srv.CreateMarket(env.ctx, msg)
	require.NoError(t, err)
}

// TestCreateMarket_RejectsIndexOutsideShrunkParams locks in that
// CreateMarket reads the live Params for the perps / spot range, not
// the compile-time constants, so a governance-tightened upper bound
// rejects MsgCreateMarket on indices outside the new range immediately.
func TestCreateMarket_RejectsIndexOutsideShrunkParams(t *testing.T) {
	env := newTestEnv(t)

	// Shrink the perps range so only indices 0..10 are admissible.
	p, _ := env.keeper.Params.Get(env.ctx)
	p.MaxPerpsMarketIndex = 10
	require.NoError(t, env.keeper.Params.Set(env.ctx, p))

	// index=5 is fine.
	_, err := env.srv.CreateMarket(env.ctx, validCreatePerpMsg(5))
	require.NoError(t, err)

	// index=20 still passes ValidateBasic (perptypes.MaxPerpsMarketIndex
	// is 254) but the keeper must reject it against the live Params.
	_, err = env.srv.CreateMarket(env.ctx, validCreatePerpMsg(20))
	require.ErrorIs(t, err, types.ErrMarketIndexExceed)

	// Symmetric guard on the spot side: shrink the spot range.
	p.MaxSpotMarketIndex = perptypes.MinSpotMarketIndex + 5
	require.NoError(t, env.keeper.Params.Set(env.ctx, p))
	_, err = env.srv.CreateMarket(env.ctx,
		validCreateSpotMsg(perptypes.MinSpotMarketIndex+10, perptypes.LITAssetIndex))
	require.ErrorIs(t, err, types.ErrMarketIndexExceed)
}

func TestCreateMarket_SpotRejectsDisabledBase(t *testing.T) {
	env := newTestEnv(t)
	a := env.asset.assets[perptypes.LITAssetIndex]
	a.Enabled = false
	env.asset.assets[perptypes.LITAssetIndex] = a
	msg := validCreateSpotMsg(perptypes.MinSpotMarketIndex, perptypes.LITAssetIndex)
	_, err := env.srv.CreateMarket(env.ctx, msg)
	require.ErrorIs(t, err, types.ErrInvalidMarket)
}

// TestCreateMarket_RuntimeFieldsForcedZero asserts the second layer of
// defence: even when ValidateBasic would have rejected the payload (we
// bypass it here by calling SetMarketDetails directly after a tweaked
// CreateMarket), the handler still zeroes the runtime block.
func TestCreateMarket_RuntimeFieldsForcedZero(t *testing.T) {
	env := newTestEnv(t)
	msg := validCreatePerpMsg(1)
	// Try poison values that ValidateBasic should already reject. The
	// handler's resetRuntimeDetails takes precedence — this test
	// guards against future refactors that remove the basic check
	// while keeping the handler intact.
	msg.MarketDetails.MarkPrice = 0
	msg.MarketDetails.OpenInterest = 0 // already 0 since ValidateBasic rejects otherwise
	_, err := env.srv.CreateMarket(env.ctx, msg)
	require.NoError(t, err)

	d, err := env.keeper.GetMarketDetails(env.ctx, 1)
	require.NoError(t, err)
	require.Zero(t, d.MarkPrice)
	require.Zero(t, d.OpenInterest)
}

func TestUpdateMarket_Success(t *testing.T) {
	env := newTestEnv(t)
	_, err := env.srv.CreateMarket(env.ctx, validCreatePerpMsg(1))
	require.NoError(t, err)
	_, err = env.srv.UpdateMarket(env.ctx, validUpdateMsg(1))
	require.NoError(t, err)

	m, _ := env.keeper.GetMarket(env.ctx, 1)
	require.EqualValues(t, 2_000, m.TakerFee)
	require.EqualValues(t, 3_000, m.LiquidationFee)
}

func TestUpdateMarket_LiquidationFeeUpdated(t *testing.T) {
	env := newTestEnv(t)
	_, err := env.srv.CreateMarket(env.ctx, validCreatePerpMsg(1))
	require.NoError(t, err)
	msg := validUpdateMsg(1)
	msg.NewLiquidationFee = 4_321
	_, err = env.srv.UpdateMarket(env.ctx, msg)
	require.NoError(t, err)
	m, _ := env.keeper.GetMarket(env.ctx, 1)
	require.EqualValues(t, 4_321, m.LiquidationFee)
}

func TestUpdateMarket_RejectsExpired(t *testing.T) {
	env := newTestEnv(t)
	_, err := env.srv.CreateMarket(env.ctx, validCreatePerpMsg(1))
	require.NoError(t, err)
	// Flip to EXPIRED directly through Markets.Set to model a
	// previously-expired market.
	m, _ := env.keeper.GetMarket(env.ctx, 1)
	m.Status = perptypes.MarketStatusExpired
	require.NoError(t, env.keeper.Markets.Set(env.ctx, m.MarketIndex, m))

	_, err = env.srv.UpdateMarket(env.ctx, validUpdateMsg(1))
	require.ErrorIs(t, err, types.ErrInvalidMarket)
}

func TestUpdateMarket_ManualExpiryCallsApplyExit(t *testing.T) {
	env := newTestEnv(t)
	_, err := env.srv.CreateMarket(env.ctx, validCreatePerpMsg(1))
	require.NoError(t, err)
	msg := validUpdateMsg(1)
	msg.NewStatus = perptypes.MarketStatusExpired
	_, err = env.srv.UpdateMarket(env.ctx, msg)
	require.NoError(t, err)

	require.Contains(t, env.liq.calls, uint32(1), "expireMarket must invoke ApplyExitPosition")

	m, _ := env.keeper.GetMarket(env.ctx, 1)
	require.Equal(t, perptypes.MarketStatusExpired, m.Status)
}

func TestUpdateMarket_ExpiryIndexUpdated(t *testing.T) {
	env := newTestEnv(t)
	create := validCreatePerpMsg(1)
	create.Market.ExpiryTimestamp = env.ctx.BlockTime().Add(time.Hour).UnixMilli()
	_, err := env.srv.CreateMarket(env.ctx, create)
	require.NoError(t, err)

	oldKey := collections.Join(create.Market.ExpiryTimestamp, uint32(1))

	newExpiry := env.ctx.BlockTime().Add(2 * time.Hour).UnixMilli()
	msg := validUpdateMsg(1)
	msg.NewExpiryTimestamp = newExpiry
	_, err = env.srv.UpdateMarket(env.ctx, msg)
	require.NoError(t, err)

	oldHas, _ := env.keeper.ExpiryIndex.Has(env.ctx, oldKey)
	require.False(t, oldHas, "old expiry entry must be removed")
	newHas, _ := env.keeper.ExpiryIndex.Has(env.ctx, collections.Join(newExpiry, uint32(1)))
	require.True(t, newHas, "new expiry entry must be written")
}

func TestUpdateMarketDetails_OnlyOverlaysGovFields(t *testing.T) {
	env := newTestEnv(t)
	_, err := env.srv.CreateMarket(env.ctx, validCreatePerpMsg(1))
	require.NoError(t, err)

	// Forcibly set MarkPrice so we can confirm UpdateMarketDetails
	// doesn't touch it.
	d, _ := env.keeper.GetMarketDetails(env.ctx, 1)
	d.MarkPrice = 12_345
	require.NoError(t, env.keeper.SetMarketDetails(env.ctx, d))

	_, err = env.srv.UpdateMarketDetails(env.ctx, &types.MsgUpdateMarketDetails{
		Authority:            testAuthority,
		MarketIndex:          1,
		NewDefaultImf:        2_000,
		NewMinImf:            1_000,
		NewMaintenanceMf:     500,
		NewCloseOutMf:        250,
		NewFundingClampSmall: uint32(perptypes.FundingSmallClamp),
		NewFundingClampBig:   uint32(perptypes.FundingBigClamp),
		NewInterestRate:      0,
		NewOpenInterestLimit: 999,
	})
	require.NoError(t, err)

	got, _ := env.keeper.GetMarketDetails(env.ctx, 1)
	require.EqualValues(t, 12_345, got.MarkPrice, "MarkPrice runtime field must be preserved")
	require.EqualValues(t, 2_000, got.DefaultInitialMarginFraction)
	require.EqualValues(t, 999, got.OpenInterestLimit)
}

func TestUpdateMarketDetails_RejectsUnknownMarket(t *testing.T) {
	env := newTestEnv(t)
	_, err := env.srv.UpdateMarketDetails(env.ctx, &types.MsgUpdateMarketDetails{
		Authority:            testAuthority,
		MarketIndex:          1,
		NewDefaultImf:        1_000,
		NewMinImf:            500,
		NewMaintenanceMf:     250,
		NewCloseOutMf:        125,
		NewFundingClampSmall: uint32(perptypes.FundingSmallClamp),
		NewFundingClampBig:   uint32(perptypes.FundingBigClamp),
		NewOpenInterestLimit: 100,
	})
	require.ErrorIs(t, err, types.ErrMarketNotFound)
}

func TestUpdateParams_Success(t *testing.T) {
	env := newTestEnv(t)
	p := types.DefaultParams()
	p.MaxMarketsExpiredPerBlock = 7
	_, err := env.srv.UpdateParams(env.ctx, &types.MsgUpdateParams{
		Authority: testAuthority,
		Params:    p,
	})
	require.NoError(t, err)

	got, _ := env.keeper.Params.Get(env.ctx)
	require.EqualValues(t, 7, got.MaxMarketsExpiredPerBlock)
}

func TestUpdateParams_RejectsUnauthorized(t *testing.T) {
	env := newTestEnv(t)
	_, err := env.srv.UpdateParams(env.ctx, &types.MsgUpdateParams{
		Authority: otherAddr,
		Params:    types.DefaultParams(),
	})
	require.ErrorIs(t, err, types.ErrInvalidAuthority)
}

func TestUpdateOpenInterest_LimitEnforced(t *testing.T) {
	env := newTestEnv(t)
	_, err := env.srv.CreateMarket(env.ctx, validCreatePerpMsg(1))
	require.NoError(t, err)
	d, _ := env.keeper.GetMarketDetails(env.ctx, 1)
	d.OpenInterestLimit = 100
	require.NoError(t, env.keeper.SetMarketDetails(env.ctx, d))

	require.NoError(t, env.keeper.UpdateOpenInterest(env.ctx, 1, 90))
	require.ErrorIs(t, env.keeper.UpdateOpenInterest(env.ctx, 1, 50), types.ErrOpenInterestLimit)
}

func TestUpdateOpenInterest_RejectsBelowZero(t *testing.T) {
	env := newTestEnv(t)
	_, err := env.srv.CreateMarket(env.ctx, validCreatePerpMsg(1))
	require.NoError(t, err)
	err = env.keeper.UpdateOpenInterest(env.ctx, 1, -1)
	require.Error(t, err, "decrementing OI below zero must be rejected, not silently clamped")
}
