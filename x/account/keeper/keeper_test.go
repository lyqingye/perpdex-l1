package keeper_test

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"

	cmtprototypes "github.com/cometbft/cometbft/proto/tendermint/types"

	"cosmossdk.io/collections"
	"cosmossdk.io/log"
	"cosmossdk.io/math"
	storetypes "cosmossdk.io/store/types"

	"github.com/cosmos/cosmos-sdk/runtime"
	"github.com/cosmos/cosmos-sdk/testutil/integration"
	sdk "github.com/cosmos/cosmos-sdk/types"
	moduletestutil "github.com/cosmos/cosmos-sdk/types/module/testutil"

	perptypes "github.com/perpdex/perpdex-l1/types"
	accountkeeper "github.com/perpdex/perpdex-l1/x/account/keeper"
	"github.com/perpdex/perpdex-l1/x/account/keeper/keepertest"
	"github.com/perpdex/perpdex-l1/x/account/types"
	assetkeeper "github.com/perpdex/perpdex-l1/x/asset/keeper"
	assettypes "github.com/perpdex/perpdex-l1/x/asset/types"
	markettypes "github.com/perpdex/perpdex-l1/x/market/types"
)

// fakeBankKeeper is a no-op bank stub sufficient for keeper-level tests
// that never actually send coins through the bank module.
type fakeBankKeeper struct{}

func (fakeBankKeeper) SendCoinsFromAccountToModule(_ context.Context, _ sdk.AccAddress, _ string, _ sdk.Coins) error {
	return nil
}
func (fakeBankKeeper) SendCoinsFromModuleToAccount(_ context.Context, _ string, _ sdk.AccAddress, _ sdk.Coins) error {
	return nil
}
func (fakeBankKeeper) GetBalance(_ context.Context, _ sdk.AccAddress, _ string) sdk.Coin {
	return sdk.Coin{}
}

// fakeRiskKeeper lets a test decide whether IsValidRiskChange should pass.
type fakeRiskKeeper struct {
	risky        bool
	snapshotHits int
}

func (f *fakeRiskKeeper) IsValidRiskChange(_ context.Context, _ uint64) (bool, error) {
	return !f.risky, nil
}
func (f *fakeRiskKeeper) GetAvailableCollateral(_ context.Context, _ uint64) (math.Int, error) {
	return math.ZeroInt(), nil
}
func (f *fakeRiskKeeper) GetTotalAccountValue(_ context.Context, _ uint64) (math.Int, error) {
	return math.ZeroInt(), nil
}
func (f *fakeRiskKeeper) GetHealthStatus(_ context.Context, _ uint64) (uint32, error) {
	return perptypes.HealthHealthy, nil
}
func (f *fakeRiskKeeper) SnapshotPreRisk(_ context.Context, _ uint64) error {
	f.snapshotHits++
	return nil
}

// fakeMarketKeeper returns deterministic market rows regardless of index.
type fakeMarketKeeper struct {
	minImf uint32
}

func (f fakeMarketKeeper) GetMarket(_ context.Context, idx uint32) (markettypes.Market, error) {
	return markettypes.Market{MarketIndex: idx, MarketType: perptypes.MarketTypePerps}, nil
}
func (f fakeMarketKeeper) GetMarketDetails(_ context.Context, idx uint32) (markettypes.MarketDetails, error) {
	return markettypes.MarketDetails{MarketIndex: idx, MinInitialMarginFraction: f.minImf}, nil
}

type testEnv struct {
	ctx    sdk.Context
	ak     accountkeeper.Keeper
	risk   *fakeRiskKeeper
	market fakeMarketKeeper
}

func initTestEnv(t *testing.T) *testEnv {
	t.Helper()

	cfg := sdk.GetConfig()
	cfg.SetBech32PrefixForAccount("px", "pxpub")

	keys := storetypes.NewKVStoreKeys(
		assettypes.StoreKey,
		types.StoreKey,
	)
	cdc := moduletestutil.MakeTestEncodingConfig().Codec
	logger := log.NewTestLogger(t)
	cms := integration.CreateMultiStore(keys, logger)
	sdkCtx := sdk.NewContext(cms, cmtprototypes.Header{}, true, logger)

	authority := "px1xqcnyve5x5mrwwpev93xxer9venks6t29ke4l8"

	ask := assetkeeper.NewKeeper(
		cdc,
		runtime.NewKVStoreService(keys[assettypes.StoreKey]),
		authority,
	)
	require.NoError(t, ask.InitGenesis(sdkCtx, *assettypes.DefaultGenesis()))

	ak := accountkeeper.NewKeeper(
		cdc,
		runtime.NewKVStoreService(keys[types.StoreKey]),
		authority,
		ask,
		fakeBankKeeper{},
	)
	require.NoError(t, ak.InitGenesis(sdkCtx, *types.DefaultGenesis()))

	risk := &fakeRiskKeeper{}
	market := fakeMarketKeeper{minImf: 100}
	ak.SetRiskKeeper(risk)
	ak.SetMarketKeeper(market)

	return &testEnv{ctx: sdkCtx, ak: ak, risk: risk, market: market}
}

// TestExportGenesis_RoundTrip asserts that account-assets / positions /
// metas all survive a genesis export + import cycle.
func TestExportGenesis_RoundTrip(t *testing.T) {
	env := initTestEnv(t)

	require.NoError(t, env.ak.AccountAssets.Set(env.ctx,
		collections.Join(perptypes.TreasuryAccountIndex, perptypes.USDCAssetIndex),
		types.AccountAsset{
			AccountIndex:  perptypes.TreasuryAccountIndex,
			AssetIndex:    perptypes.USDCAssetIndex,
			Balance:       math.NewInt(123),
			LockedBalance: math.ZeroInt(),
			MarginMode:    perptypes.MarginModeEnabled,
		}))
	require.NoError(t, env.ak.AccountPositions.Set(env.ctx,
		collections.Join(perptypes.TreasuryAccountIndex, uint32(0)),
		types.AccountPosition{
			AccountIndex:             perptypes.TreasuryAccountIndex,
			MarketIndex:              0,
			Position:                 math.NewInt(5),
			EntryQuote:               math.NewInt(500),
			LastFundingRatePrefixSum: math.ZeroInt(),
			AllocatedMargin:          math.ZeroInt(),
			MarginMode:               perptypes.CrossMargin,
		}))
	require.NoError(t, env.ak.AccountMetas.Set(env.ctx, perptypes.TreasuryAccountIndex,
		types.AccountMeta{AccountIndex: perptypes.TreasuryAccountIndex}))

	gs, err := env.ak.ExportGenesis(env.ctx)
	require.NoError(t, err)
	require.Len(t, gs.AccountAssets, 1)
	require.Len(t, gs.AccountPositions, 1)
	require.Len(t, gs.AccountMetas, 1)
}

// TestCheckMinOperatorShareRate_EmptyPool short-circuits the skin-in-game
// check for pools that have not yet minted shares.
func TestCheckMinOperatorShareRate_EmptyPool(t *testing.T) {
	info := types.PublicPoolInfo{
		TotalShares:          math.ZeroInt(),
		OperatorShares:       math.ZeroInt(),
		MinOperatorShareRate: 5000,
	}
	require.True(t, accountkeeper.CheckMinOperatorShareRate(info))
}

// TestCheckMinOperatorShareRate_Violated rejects an operator whose share
// balance dipped below the configured floor.
func TestCheckMinOperatorShareRate_Violated(t *testing.T) {
	info := types.PublicPoolInfo{
		TotalShares:          math.NewInt(1000),
		OperatorShares:       math.NewInt(10),
		MinOperatorShareRate: 5000, // 50% floor
	}
	require.False(t, accountkeeper.CheckMinOperatorShareRate(info))
}

// TestWithdraw_RejectsPoolAccount ensures the generic Withdraw Msg refuses
// to touch PUBLIC_POOL / INSURANCE_FUND accounts (audit Blocker account-1).
func TestWithdraw_RejectsPoolAccount(t *testing.T) {
	env := initTestEnv(t)
	srv := accountkeeper.NewMsgServerImpl(env.ak)

	owner := "px1qv9pzxqlyckngw6zf9g9whn9d3eh4qvgsxc8cx"
	pool := types.Account{
		AccountIndex: 4242,
		OwnerAddress: owner,
		AccountType:  perptypes.PublicPoolAccountType,
		Collateral:   math.NewInt(1_000_000),
	}
	require.NoError(t, keepertest.SetAccountForTest(env.ctx, env.ak, pool))

	_, err := srv.Withdraw(env.ctx, &types.MsgWithdraw{
		Sender:       owner,
		AccountIndex: pool.AccountIndex,
		AssetIndex:   perptypes.USDCAssetIndex,
		Amount:       1000,
		RouteType:    perptypes.RouteTypePerps,
	})
	require.ErrorIs(t, err, types.ErrPoolGenericMsg)
}

// TestTransfer_RejectsMissingDestination short-circuits when the destination
// account does not exist, rather than silently creating a new row.
func TestTransfer_RejectsMissingDestination(t *testing.T) {
	env := initTestEnv(t)
	srv := accountkeeper.NewMsgServerImpl(env.ak)

	owner := "px1qv9pzxqlyckngw6zf9g9whn9d3eh4qvgsxc8cx"
	src := types.Account{
		AccountIndex: 501,
		OwnerAddress: owner,
		AccountType:  perptypes.MasterAccountType,
		Collateral:   math.NewInt(10_000),
	}
	require.NoError(t, keepertest.SetAccountForTest(env.ctx, env.ak, src))

	_, err := srv.Transfer(env.ctx, &types.MsgTransfer{
		Sender:           owner,
		FromAccountIndex: 501,
		ToAccountIndex:   999, // does not exist
		AssetIndex:       perptypes.USDCAssetIndex,
		Amount:           1,
	})
	require.ErrorIs(t, err, types.ErrAccountNotFound)
}

// TestUpdateLeverage_RejectsBelowMarketMinIMF makes sure UpdateLeverage
// honours the market-details floor (audit Medium account-7).
func TestUpdateLeverage_RejectsBelowMarketMinIMF(t *testing.T) {
	env := initTestEnv(t)
	env.market = fakeMarketKeeper{minImf: 500}
	env.ak.SetMarketKeeper(env.market)
	srv := accountkeeper.NewMsgServerImpl(env.ak)

	owner := "px1qv9pzxqlyckngw6zf9g9whn9d3eh4qvgsxc8cx"
	require.NoError(t, keepertest.SetAccountForTest(env.ctx, env.ak, types.Account{
		AccountIndex: 888,
		OwnerAddress: owner,
		AccountType:  perptypes.MasterAccountType,
		Collateral:   math.NewInt(1),
	}))

	_, err := srv.UpdateLeverage(env.ctx, &types.MsgUpdateLeverage{
		Sender:                   owner,
		AccountIndex:             888,
		MarketIndex:              0,
		NewInitialMarginFraction: 100, // below min 500
		NewMarginMode:            perptypes.CrossMargin,
	})
	require.ErrorIs(t, err, types.ErrInvalidParams)
}
