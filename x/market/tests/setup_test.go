// Package tests groups every market-module behaviour test into a single
// external test package so production code in x/market/keeper and
// x/market/types stays free of *_test.go siblings. The file below owns
// the chain-wide TestMain bootstrap, in-memory keeper fixture, asset /
// liquidation stubs and message builders that every other test in this
// directory shares.
package tests

import (
	"context"
	"errors"
	"os"
	"testing"
	"time"

	cmtprototypes "github.com/cometbft/cometbft/proto/tendermint/types"
	"github.com/stretchr/testify/require"

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
	// testAuthority is the canonical bech32 address used as the
	// market module's authority across every test in this package.
	testAuthority = "px1xqcnyve5x5mrwwpev93xxer9venks6t29ke4l8"
	// otherAddr is a second valid bech32 address used to drive the
	// "wrong authority" rejection branches.
	otherAddr = "px1qv9pzxqlyckngw6zf9g9whn9d3eh4qvgsxc8cx"
)

// TestMain configures the chain-wide `px` bech32 prefix once per
// process so all msg- and keeper-level tests resolve the canonical
// authority address consistently.
func TestMain(m *testing.M) {
	cfg := sdk.GetConfig()
	cfg.SetBech32PrefixForAccount("px", "pxpub")
	cfg.SetBech32PrefixForValidator("pxvaloper", "pxvaloperpub")
	cfg.SetBech32PrefixForConsensusNode("pxvalcons", "pxvalconspub")
	os.Exit(m.Run())
}

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

// testEnv bundles an in-memory keeper, its msg/query servers and the
// asset/liquidation stubs so every test can drive the module end-to-end
// without dragging the full app fixture along.
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

// validCreatePerpMsg builds a MsgCreateMarket that passes both
// ValidateBasic and the keeper-side checks for a perp at the given
// index. Tests mutate the returned message to drive specific rejection
// branches.
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

// validCreateSpotMsg switches a perp template to spot semantics for the
// given index and base asset.
func validCreateSpotMsg(idx uint32, baseAsset uint32) *types.MsgCreateMarket {
	m := validCreatePerpMsg(idx)
	m.Market.MarketIndex = idx
	m.Market.MarketType = perptypes.MarketTypeSpot
	m.Market.BaseAssetId = baseAsset
	m.MarketDetails.MarketIndex = idx
	return m
}

// validUpdateMsg is the keeper-level UpdateMarket builder used by
// lifecycle tests that exercise the full msg server path. msgs_test
// uses validUpdateMarketMsg for the ValidateBasic-only flavour.
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

// validPerpMarket returns a perp market record that should pass every
// statics check at the canonical defaults. Tests mutate one field per
// subcase to assert each rejection path independently.
func validPerpMarket() types.Market {
	return types.Market{
		MarketIndex:              1,
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
		ExpiryTimestamp:          0,
	}
}

func validSpotMarket() types.Market {
	m := validPerpMarket()
	m.MarketIndex = perptypes.MinSpotMarketIndex
	m.MarketType = perptypes.MarketTypeSpot
	m.BaseAssetId = perptypes.NativeAssetIndex
	m.QuoteAssetId = perptypes.USDCAssetIndex
	return m
}

func validDetailsInit(marketIndex uint32) types.MarketDetails {
	return types.MarketDetails{
		MarketIndex:                  marketIndex,
		DefaultInitialMarginFraction: 1_000, // 10%
		MinInitialMarginFraction:     500,   // 5%
		MaintenanceMarginFraction:    250,   // 2.5%
		CloseOutMarginFraction:       125,   // 1.25%
		FundingClampSmall:            uint32(perptypes.FundingSmallClamp),
		FundingClampBig:              uint32(perptypes.FundingBigClamp),
		InterestRate:                 0,
		OpenInterestLimit:            1_000_000,
		AskNonce:                     perptypes.FirstAskNonce,
		BidNonce:                     perptypes.FirstBidNonce,
		FundingRatePrefixSum:         math.ZeroInt(),
	}
}

func validUpdateMarketMsg() *types.MsgUpdateMarket {
	return &types.MsgUpdateMarket{
		Authority:          testAuthority,
		MarketIndex:        1,
		NewStatus:          perptypes.MarketStatusActive,
		NewTakerFee:        100,
		NewMakerFee:        50,
		NewLiquidationFee:  200,
		NewMinBaseAmount:   1,
		NewMinQuoteAmount:  1,
		NewOrderQuoteLimit: 0,
		NewExpiryTimestamp: 0,
	}
}

func validUpdateMarketDetailsMsg() *types.MsgUpdateMarketDetails {
	return &types.MsgUpdateMarketDetails{
		Authority:            testAuthority,
		MarketIndex:          1,
		NewDefaultImf:        1_000,
		NewMinImf:            500,
		NewMaintenanceMf:     250,
		NewCloseOutMf:        125,
		NewFundingClampSmall: uint32(perptypes.FundingSmallClamp),
		NewFundingClampBig:   uint32(perptypes.FundingBigClamp),
		NewInterestRate:      0,
		NewOpenInterestLimit: 1_000_000,
	}
}
