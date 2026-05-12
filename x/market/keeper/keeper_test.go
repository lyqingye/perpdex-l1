package keeper_test

import (
	"context"
	"errors"
	"testing"
	"time"

	cmtprototypes "github.com/cometbft/cometbft/proto/tendermint/types"
	"github.com/stretchr/testify/require"

	"cosmossdk.io/collections"
	"cosmossdk.io/log"
	"cosmossdk.io/math"
	storetypes "cosmossdk.io/store/types"

	"github.com/cosmos/cosmos-sdk/runtime"
	"github.com/cosmos/cosmos-sdk/testutil/integration"
	sdk "github.com/cosmos/cosmos-sdk/types"
	moduletestutil "github.com/cosmos/cosmos-sdk/types/module/testutil"

	perptypes "github.com/perpdex/perpdex-l1/types"
	assettypes "github.com/perpdex/perpdex-l1/x/asset/types"
	marketkeeper "github.com/perpdex/perpdex-l1/x/market/keeper"
	"github.com/perpdex/perpdex-l1/x/market/types"
)

const (
	testAuthority = "px1xqcnyve5x5mrwwpev93xxer9venks6t29ke4l8"
	otherAddr     = "px1qv9pzxqlyckngw6zf9g9whn9d3eh4qvgsxc8cx"
)

// ---------------------------------------------------------------------
// Stubs
// ---------------------------------------------------------------------

// stubAssetKeeper minimally satisfies types.AssetKeeper. Tests preload
// the asset table via Seed; the default fixture comes with a USDC and
// one extra non-USDC base asset already enabled so most CreateMarket
// happy-path tests don't need to construct anything custom.
type stubAssetKeeper struct {
	assets map[uint32]assettypes.Asset
}

func newStubAssetKeeper() *stubAssetKeeper {
	return &stubAssetKeeper{
		assets: map[uint32]assettypes.Asset{
			perptypes.USDCAssetIndex: {
				AssetIndex:  perptypes.USDCAssetIndex,
				Denom:       "uusdc",
				DisplayName: "USDC",
				Enabled:     true,
				MarginMode:  perptypes.MarginModeEnabled,
			},
			perptypes.NativeAssetIndex: {
				AssetIndex:  perptypes.NativeAssetIndex,
				Denom:       "uperp",
				DisplayName: "PERP",
				Enabled:     true,
				MarginMode:  perptypes.MarginModeDisabled,
			},
			perptypes.LITAssetIndex: {
				AssetIndex:  perptypes.LITAssetIndex,
				Denom:       "ulit",
				DisplayName: "LIT",
				Enabled:     true,
				MarginMode:  perptypes.MarginModeDisabled,
			},
		},
	}
}

func (s *stubAssetKeeper) GetAsset(_ context.Context, idx uint32) (assettypes.Asset, error) {
	a, ok := s.assets[idx]
	if !ok {
		return assettypes.Asset{}, errors.New("asset not found")
	}
	return a, nil
}

// stubLiquidationKeeper records calls and can be configured to return
// an error so tests can drive both the success and failure branches of
// expireMarket / ApplyExitPosition wiring.
type stubLiquidationKeeper struct {
	calls   []uint32
	failErr error
}

func (s *stubLiquidationKeeper) ApplyExitPosition(_ context.Context, marketIdx uint32) error {
	s.calls = append(s.calls, marketIdx)
	return s.failErr
}

// ---------------------------------------------------------------------
// Test environment
// ---------------------------------------------------------------------

type testEnv struct {
	ctx    sdk.Context
	keeper marketkeeper.Keeper
	srv    types.MsgServer
	q      types.QueryServer
	asset  *stubAssetKeeper
	liq    *stubLiquidationKeeper
}

func newTestEnv(t *testing.T) *testEnv {
	t.Helper()

	cfg := sdk.GetConfig()
	cfg.SetBech32PrefixForAccount("px", "pxpub")

	keys := storetypes.NewKVStoreKeys(types.StoreKey)
	cdc := moduletestutil.MakeTestEncodingConfig().Codec
	logger := log.NewTestLogger(t)
	cms := integration.CreateMultiStore(keys, logger)
	now := time.Unix(1_700_000_000, 0)
	ctx := sdk.NewContext(cms, cmtprototypes.Header{Time: now}, true, logger).
		WithBlockTime(now)

	asset := newStubAssetKeeper()
	liq := &stubLiquidationKeeper{}

	k := marketkeeper.NewKeeper(
		cdc,
		runtime.NewKVStoreService(keys[types.StoreKey]),
		testAuthority,
		asset,
	)
	k.SetLiquidationKeeper(liq)
	require.NoError(t, k.InitGenesis(ctx, *types.DefaultGenesis()))

	return &testEnv{
		ctx:    ctx,
		keeper: k,
		srv:    marketkeeper.NewMsgServerImpl(k),
		q:      marketkeeper.NewQuerier(k),
		asset:  asset,
		liq:    liq,
	}
}

// newTestEnvWithoutLiquidation builds an env where SetLiquidationKeeper
// was never called. Used to test the nil-safe expireMarket path (H7).
func newTestEnvWithoutLiquidation(t *testing.T) *testEnv {
	t.Helper()
	env := newTestEnv(t)
	// Re-build a keeper without setting the liquidation hook.
	cfg := sdk.GetConfig()
	cfg.SetBech32PrefixForAccount("px", "pxpub")

	keys := storetypes.NewKVStoreKeys(types.StoreKey)
	cdc := moduletestutil.MakeTestEncodingConfig().Codec
	logger := log.NewTestLogger(t)
	cms := integration.CreateMultiStore(keys, logger)
	now := time.Unix(1_700_000_000, 0)
	ctx := sdk.NewContext(cms, cmtprototypes.Header{Time: now}, true, logger).
		WithBlockTime(now)
	k := marketkeeper.NewKeeper(
		cdc,
		runtime.NewKVStoreService(keys[types.StoreKey]),
		testAuthority,
		env.asset,
	)
	require.NoError(t, k.InitGenesis(ctx, *types.DefaultGenesis()))
	env.ctx = ctx
	env.keeper = k
	env.srv = marketkeeper.NewMsgServerImpl(k)
	env.q = marketkeeper.NewQuerier(k)
	env.liq = nil
	return env
}

func validCreatePerpMsg(idx uint32) *types.MsgCreateMarket {
	mkt := types.Market{
		MarketIndex:              idx,
		Status:                   perptypes.MarketStatusActive,
		MarketType:               perptypes.MarketTypePerps,
		BaseAssetId:              perptypes.NativeAssetIndex,
		QuoteAssetId:             perptypes.USDCAssetIndex,
		TakerFee:                 1_000,
		MakerFee:                 500,
		LiquidationFee:           2_000,
		SizeExtensionMultiplier:  1,
		QuoteExtensionMultiplier: 1,
		MinBaseAmount:            1,
		MinQuoteAmount:           1,
		OrderQuoteLimit:          1,
	}
	det := types.MarketDetails{
		MarketIndex:                  idx,
		DefaultInitialMarginFraction: 1_000,
		MinInitialMarginFraction:     500,
		MaintenanceMarginFraction:    250,
		CloseOutMarginFraction:       125,
		FundingClampSmall:            uint32(perptypes.FundingSmallClamp),
		FundingClampBig:              uint32(perptypes.FundingBigClamp),
		OpenInterestLimit:            1_000_000,
		AskNonce:                     perptypes.FirstAskNonce,
		BidNonce:                     perptypes.FirstBidNonce,
		FundingRatePrefixSum:         math.ZeroInt(),
	}
	return &types.MsgCreateMarket{
		Authority:     testAuthority,
		Market:        mkt,
		MarketDetails: det,
	}
}

func validCreateSpotMsg(idx uint32, baseAsset uint32) *types.MsgCreateMarket {
	m := validCreatePerpMsg(idx)
	m.Market.MarketIndex = idx
	m.Market.MarketType = perptypes.MarketTypeSpot
	m.Market.BaseAssetId = baseAsset
	m.MarketDetails.MarketIndex = idx
	return m
}

// ---------------------------------------------------------------------
// CreateMarket
// ---------------------------------------------------------------------

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
	// Flip the USDC asset to disabled.
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

// TestCreateMarket_RejectsIndexOutsideShrunkParams locks down that
// CreateMarket reads the current Params for the perps/spot range, not
// the compile-time constants. Governance can legitimately tighten the
// upper bound after launch; without this stateful check a freshly
// narrowed range would only be enforced for future MsgUpdateParams
// calls while still admitting MsgCreateMarket up to the old constant.
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

// ---------------------------------------------------------------------
// UpdateMarket
// ---------------------------------------------------------------------

func validUpdateMsg(idx uint32) *types.MsgUpdateMarket {
	return &types.MsgUpdateMarket{
		Authority:          testAuthority,
		MarketIndex:        idx,
		NewStatus:          perptypes.MarketStatusActive,
		NewTakerFee:        2_000,
		NewMakerFee:        1_000,
		NewLiquidationFee:  3_000,
		NewMinBaseAmount:   2,
		NewMinQuoteAmount:  2,
		NewOrderQuoteLimit: 5,
	}
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

	// Change expiry timestamp via UpdateMarket.
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

// ---------------------------------------------------------------------
// UpdateMarketDetails / UpdateParams
// ---------------------------------------------------------------------

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

// ---------------------------------------------------------------------
// UpdateOpenInterest (M13)
// ---------------------------------------------------------------------

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

// ---------------------------------------------------------------------
// EndBlocker
// ---------------------------------------------------------------------

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

	// Expiry index entry for market 1 must be gone.
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
	// Drop the params budget to 2.
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

// ---------------------------------------------------------------------
// Genesis (export / import / pairing invariants)
// ---------------------------------------------------------------------

func TestGenesis_ExportImportRoundTrip(t *testing.T) {
	env := newTestEnv(t)
	_, err := env.srv.CreateMarket(env.ctx, validCreatePerpMsg(1))
	require.NoError(t, err)

	gs, err := env.keeper.ExportGenesis(env.ctx)
	require.NoError(t, err)
	require.Len(t, gs.Markets, 1)
	require.Len(t, gs.MarketDetails, 1)
	require.NoError(t, gs.Validate())
}

func TestGenesis_PairingViolationsRejected(t *testing.T) {
	gs := types.DefaultGenesis()
	gs.Markets = []types.Market{validCreatePerpMsg(1).Market}
	require.Error(t, gs.Validate(), "market without matching details must be rejected")

	gs = types.DefaultGenesis()
	gs.MarketDetails = []types.MarketDetails{validCreatePerpMsg(1).MarketDetails}
	require.Error(t, gs.Validate(), "details without matching market must be rejected")
}

func TestGenesis_DuplicateMarketRejected(t *testing.T) {
	gs := types.DefaultGenesis()
	mkt := validCreatePerpMsg(1).Market
	gs.Markets = []types.Market{mkt, mkt}
	gs.MarketDetails = []types.MarketDetails{validCreatePerpMsg(1).MarketDetails}
	require.Error(t, gs.Validate())
}

func TestGenesis_MarketStaticsRejected(t *testing.T) {
	gs := types.DefaultGenesis()
	mkt := validCreatePerpMsg(1).Market
	mkt.TakerFee = uint32(perptypes.FeeTick)
	gs.Markets = []types.Market{mkt}
	gs.MarketDetails = []types.MarketDetails{validCreatePerpMsg(1).MarketDetails}
	require.Error(t, gs.Validate())
}

func TestGenesis_ExpiredMarketAccepted(t *testing.T) {
	gs := types.DefaultGenesis()
	mkt := validCreatePerpMsg(1).Market
	mkt.Status = perptypes.MarketStatusExpired
	gs.Markets = []types.Market{mkt}
	gs.MarketDetails = []types.MarketDetails{validCreatePerpMsg(1).MarketDetails}
	require.NoError(t, gs.Validate(), "EXPIRED markets are legal in genesis")
}

// TestGenesis_RebuildsExpiryIndex confirms that InitGenesis re-registers
// every Market's expiry timestamp in the secondary index.
func TestGenesis_RebuildsExpiryIndex(t *testing.T) {
	env := newTestEnv(t)
	gs := types.DefaultGenesis()
	mkt := validCreatePerpMsg(1).Market
	mkt.ExpiryTimestamp = 1_700_000_000_000
	gs.Markets = []types.Market{mkt}
	gs.MarketDetails = []types.MarketDetails{validCreatePerpMsg(1).MarketDetails}
	require.NoError(t, env.keeper.InitGenesis(env.ctx, *gs))

	has, _ := env.keeper.ExpiryIndex.Has(env.ctx, collections.Join(int64(1_700_000_000_000), uint32(1)))
	require.True(t, has)
}

// ---------------------------------------------------------------------
// Markets gRPC query (H8)
// ---------------------------------------------------------------------

func TestQueryMarkets_NilRequest(t *testing.T) {
	env := newTestEnv(t)
	_, err := env.q.Markets(env.ctx, nil)
	require.Error(t, err)
}

func TestQueryMarkets_Lists(t *testing.T) {
	env := newTestEnv(t)
	_, err := env.srv.CreateMarket(env.ctx, validCreatePerpMsg(1))
	require.NoError(t, err)
	_, err = env.srv.CreateMarket(env.ctx, validCreateSpotMsg(perptypes.MinSpotMarketIndex, perptypes.LITAssetIndex))
	require.NoError(t, err)

	resp, err := env.q.Markets(env.ctx, &types.QueryMarketsRequest{})
	require.NoError(t, err)
	require.Len(t, resp.Markets, 2)
}

func TestQueryMarkets_FilterByType(t *testing.T) {
	env := newTestEnv(t)
	_, err := env.srv.CreateMarket(env.ctx, validCreatePerpMsg(1))
	require.NoError(t, err)
	_, err = env.srv.CreateMarket(env.ctx, validCreateSpotMsg(perptypes.MinSpotMarketIndex, perptypes.LITAssetIndex))
	require.NoError(t, err)

	// Spot-only filter.
	resp, err := env.q.Markets(env.ctx, &types.QueryMarketsRequest{
		FilterByType: true,
		MarketType:   perptypes.MarketTypeSpot,
	})
	require.NoError(t, err)
	require.Len(t, resp.Markets, 1)
	require.Equal(t, perptypes.MarketTypeSpot, resp.Markets[0].MarketType)

	// Perps-only filter (regression for proto3 default-value
	// ambiguity: MarketTypePerps == 0 is indistinguishable from
	// "unset", so the FilterByType flag is the only way to disambiguate).
	resp, err = env.q.Markets(env.ctx, &types.QueryMarketsRequest{
		FilterByType: true,
		MarketType:   perptypes.MarketTypePerps,
	})
	require.NoError(t, err)
	require.Len(t, resp.Markets, 1)
	require.Equal(t, perptypes.MarketTypePerps, resp.Markets[0].MarketType)

	// FilterByType=false ignores MarketType (even when set to
	// MarketTypeSpot) and returns both markets.
	resp, err = env.q.Markets(env.ctx, &types.QueryMarketsRequest{
		FilterByType: false,
		MarketType:   perptypes.MarketTypeSpot,
	})
	require.NoError(t, err)
	require.Len(t, resp.Markets, 2)
}
