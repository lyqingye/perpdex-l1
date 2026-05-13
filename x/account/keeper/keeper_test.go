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
	"github.com/cosmos/gogoproto/proto"

	perptypes "github.com/perpdex/perpdex-l1/types"
	accountkeeper "github.com/perpdex/perpdex-l1/x/account/keeper"
	"github.com/perpdex/perpdex-l1/x/account/keeper/keepertest"
	"github.com/perpdex/perpdex-l1/x/account/types"
	assetkeeper "github.com/perpdex/perpdex-l1/x/asset/keeper"
	assettypes "github.com/perpdex/perpdex-l1/x/asset/types"
	markettypes "github.com/perpdex/perpdex-l1/x/market/types"
	risktypes "github.com/perpdex/perpdex-l1/x/risk/types"
)

// resetEvents clears the test ctx's event manager so a follow-up
// assertion only inspects events fired by the next operation.
func resetEvents(env *testEnv) {
	env.ctx = env.ctx.WithEventManager(sdk.NewEventManager())
}

// countEvents returns the number of events on env.ctx whose Type
// matches the protobuf message name of the supplied typed event.
// Typed events are flattened into sdk.Event{Type: proto.MessageName(msg)}
// by EventManager.EmitTypedEvent.
func countEvents(env *testEnv, msg proto.Message) int {
	want := proto.MessageName(msg)
	n := 0
	for _, ev := range env.ctx.EventManager().Events() {
		if ev.Type == want {
			n++
		}
	}
	return n
}

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

// fakeRiskKeeper lets a test decide whether IsValidRiskChangeFrom should pass.
type fakeRiskKeeper struct {
	risky        bool
	snapshotHits int
}

func (f *fakeRiskKeeper) IsValidRiskChangeFrom(_ context.Context, _ uint64, _ risktypes.PreRiskSnapshot) (bool, error) {
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
func (f *fakeRiskKeeper) SnapshotRisk(_ context.Context, _ uint64) (risktypes.PreRiskSnapshot, error) {
	f.snapshotHits++
	return risktypes.PreRiskSnapshot{}, nil
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
			BaseSize:                 math.NewInt(5),
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

// TestWithdraw_RejectsPoolAccount pins the invariant that the generic
// Withdraw Msg refuses to touch PUBLIC_POOL / INSURANCE_FUND accounts.
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

// internalAmount converts an external USDC base amount (6 decimals) to the
// internal collateral-precision delta used by spot AccountAsset rows.
func internalAmount(externalAmount uint64) math.Int {
	return math.NewIntFromUint64(externalAmount).
		Mul(math.NewIntFromUint64(perptypes.USDCToCollateralMultiplier))
}

// TestWithdraw_RespectsSpotLock validates that the spot Withdraw path
// cannot drain a balance below the resting LockedBalance reservation
// (audit H1: AddAccountAssetBalance must enforce Available on debit).
func TestWithdraw_RespectsSpotLock(t *testing.T) {
	env := initTestEnv(t)
	srv := accountkeeper.NewMsgServerImpl(env.ak)

	owner := "px1qv9pzxqlyckngw6zf9g9whn9d3eh4qvgsxc8cx"
	require.NoError(t, keepertest.SetAccountForTest(env.ctx, env.ak, types.Account{
		AccountIndex: 1001,
		OwnerAddress: owner,
		AccountType:  perptypes.MasterAccountType,
		Collateral:   math.ZeroInt(),
	}))
	// Set spot row with Balance large enough to satisfy minimum but
	// LockedBalance leaving only 5M USDC available, well under the
	// 10M minimum withdrawal. Without H1 the debit would succeed and
	// leave Balance < LockedBalance.
	require.NoError(t, keepertest.SetAccountAssetForTest(env.ctx, env.ak, types.AccountAsset{
		AccountIndex:  1001,
		AssetIndex:    perptypes.USDCAssetIndex,
		Balance:       internalAmount(15_000_000),
		LockedBalance: internalAmount(10_000_000),
		MarginMode:    perptypes.MarginModeEnabled,
	}))

	_, err := srv.Withdraw(env.ctx, &types.MsgWithdraw{
		Sender:       owner,
		AccountIndex: 1001,
		AssetIndex:   perptypes.USDCAssetIndex,
		Amount:       10_000_000,
		RouteType:    perptypes.RouteTypeSpot,
	})
	require.ErrorIs(t, err, types.ErrInsufficientFunds)
}

// TestAddAccountAssetBalance_RespectsLock isolates H1: a negative
// delta cannot drain Balance below the resting LockedBalance, even
// though the post-state Balance would still be non-negative. This is
// the invariant that protects spot Withdraw / Transfer from raiding
// resting spot order locks.
func TestAddAccountAssetBalance_RespectsLock(t *testing.T) {
	env := initTestEnv(t)

	require.NoError(t, keepertest.SetAccountAssetForTest(env.ctx, env.ak, types.AccountAsset{
		AccountIndex:  2001,
		AssetIndex:    perptypes.USDCAssetIndex,
		Balance:       internalAmount(20),
		LockedBalance: internalAmount(15),
		MarginMode:    perptypes.MarginModeDisabled,
	}))
	require.ErrorIs(t,
		env.ak.AddAccountAssetBalance(env.ctx, 2001, perptypes.USDCAssetIndex, internalAmount(10).Neg()),
		types.ErrInsufficientFunds,
	)
	// Withdrawing within Available succeeds.
	require.NoError(t,
		env.ak.AddAccountAssetBalance(env.ctx, 2001, perptypes.USDCAssetIndex, internalAmount(5).Neg()),
	)
}

// TestWithdraw_RespectsParamsMin proves the module-level Params floor
// is now actually consulted on Withdraw (audit M1).
func TestWithdraw_RespectsParamsMin(t *testing.T) {
	env := initTestEnv(t)
	srv := accountkeeper.NewMsgServerImpl(env.ak)

	owner := "px1qv9pzxqlyckngw6zf9g9whn9d3eh4qvgsxc8cx"
	require.NoError(t, keepertest.SetAccountForTest(env.ctx, env.ak, types.Account{
		AccountIndex: 3001,
		OwnerAddress: owner,
		AccountType:  perptypes.MasterAccountType,
		Collateral:   internalAmount(1_000_000_000),
	}))
	// Inflate the module-level minimum well above the asset min so the
	// post-fix branch fires.
	require.NoError(t, env.ak.Params.Set(env.ctx, types.Params{
		MinPartialTransferAmount:      perptypes.MinPartialTransferAmount,
		MinPartialWithdrawAmount:      perptypes.MinPartialWithdrawAmount * 5,
		LiquidityPoolIndex:            perptypes.InsuranceFundOperatorAccountIdx,
		LiquidityPoolCooldownPeriodMs: perptypes.DefaultLLPCooldownPeriodMs,
	}))

	_, err := srv.Withdraw(env.ctx, &types.MsgWithdraw{
		Sender:       owner,
		AccountIndex: 3001,
		AssetIndex:   perptypes.USDCAssetIndex,
		Amount:       perptypes.MinPartialWithdrawAmount * 2, // above asset min but below new params min
		RouteType:    perptypes.RouteTypePerps,
	})
	require.ErrorIs(t, err, types.ErrAmountTooSmall)
}

// TestTransfer_RespectsParamsMin proves Transfer now enforces a
// minimum amount (audit M2 + M1).
func TestTransfer_RespectsParamsMin(t *testing.T) {
	env := initTestEnv(t)
	srv := accountkeeper.NewMsgServerImpl(env.ak)

	owner := "px1qv9pzxqlyckngw6zf9g9whn9d3eh4qvgsxc8cx"
	require.NoError(t, keepertest.SetAccountForTest(env.ctx, env.ak, types.Account{
		AccountIndex: 4001,
		OwnerAddress: owner,
		AccountType:  perptypes.MasterAccountType,
		Collateral:   internalAmount(1_000_000_000),
	}))
	require.NoError(t, keepertest.SetAccountForTest(env.ctx, env.ak, types.Account{
		AccountIndex: 4002,
		OwnerAddress: owner,
		AccountType:  perptypes.MasterAccountType,
		Collateral:   math.ZeroInt(),
	}))

	_, err := srv.Transfer(env.ctx, &types.MsgTransfer{
		Sender:           owner,
		FromAccountIndex: 4001,
		ToAccountIndex:   4002,
		AssetIndex:       perptypes.USDCAssetIndex,
		Amount:           1, // way below MinPartialTransferAmount
	})
	require.ErrorIs(t, err, types.ErrAmountTooSmall)
}

// TestIsAuthorized_RejectsEmptyOwner ensures owner-less genesis accounts
// (treasury / IF) cannot be matched by an empty signer (audit M6).
func TestIsAuthorized_RejectsEmptyOwner(t *testing.T) {
	env := initTestEnv(t)

	require.NoError(t, keepertest.SetAccountForTest(env.ctx, env.ak, types.Account{
		AccountIndex: 9999,
		OwnerAddress: "",
		AccountType:  perptypes.MasterAccountType,
	}))

	ok, err := env.ak.IsAuthorized(env.ctx, "", 9999)
	require.NoError(t, err)
	require.False(t, ok)
	// Sanity: a non-empty signer also fails on an owner-less row.
	ok, err = env.ak.IsAuthorized(env.ctx, "px1someaddr", 9999)
	require.NoError(t, err)
	require.False(t, ok)
}

// TestSubAccounts_UsesSecondaryIndex confirms the SubAccounts query is
// populated via the master->sub keyset rather than scanning every
// account (audit M4).
func TestSubAccounts_UsesSecondaryIndex(t *testing.T) {
	env := initTestEnv(t)
	srv := accountkeeper.NewQuerier(env.ak)

	// Wire two masters with non-overlapping sub-accounts and validate
	// the query only returns the requested master's children.
	owner := "px1qv9pzxqlyckngw6zf9g9whn9d3eh4qvgsxc8cx"
	const masterA, masterB uint64 = 5001, 5002
	require.NoError(t, keepertest.SetAccountForTest(env.ctx, env.ak, types.Account{
		AccountIndex: masterA, OwnerAddress: owner, AccountType: perptypes.MasterAccountType,
	}))
	require.NoError(t, keepertest.SetAccountForTest(env.ctx, env.ak, types.Account{
		AccountIndex: masterB, OwnerAddress: owner, AccountType: perptypes.MasterAccountType,
	}))
	for _, sub := range []uint64{6001, 6002, 6003} {
		require.NoError(t, keepertest.SetAccountForTest(env.ctx, env.ak, types.Account{
			AccountIndex:       sub,
			MasterAccountIndex: masterA,
			OwnerAddress:       owner,
			AccountType:        perptypes.SubAccountType,
		}))
	}
	require.NoError(t, keepertest.SetAccountForTest(env.ctx, env.ak, types.Account{
		AccountIndex:       7001,
		MasterAccountIndex: masterB,
		OwnerAddress:       owner,
		AccountType:        perptypes.SubAccountType,
	}))

	resp, err := srv.SubAccounts(env.ctx, &types.QuerySubAccountsRequest{MasterAccountIndex: masterA})
	require.NoError(t, err)
	require.Len(t, resp.Accounts, 3)
	for _, a := range resp.Accounts {
		require.Equal(t, masterA, a.MasterAccountIndex)
	}
}

// TestMintShares_BlockedByRiskRegression verifies the master's
// post-state risk is enforced on MintShares (audit H2).
func TestMintShares_BlockedByRiskRegression(t *testing.T) {
	env := initTestEnv(t)
	srv := accountkeeper.NewMsgServerImpl(env.ak)

	owner := "px1qv9pzxqlyckngw6zf9g9whn9d3eh4qvgsxc8cx"
	const masterIdx uint64 = 8001
	require.NoError(t, keepertest.SetAccountForTest(env.ctx, env.ak, types.Account{
		AccountIndex: masterIdx,
		OwnerAddress: owner,
		AccountType:  perptypes.MasterAccountType,
		Collateral:   internalAmount(1_000_000_000),
	}))
	// Seed a public pool with non-zero total shares so USDCValueToShares
	// uses the NAV branch (which calls riskKeeper.GetTotalAccountValue).
	require.NoError(t, keepertest.SetAccountForTest(env.ctx, env.ak, types.Account{
		AccountIndex:       9001,
		MasterAccountIndex: masterIdx,
		OwnerAddress:       owner,
		AccountType:        perptypes.PublicPoolAccountType,
		Collateral:         internalAmount(1_000),
		PublicPoolInfo: &types.PublicPoolInfo{
			Status:               perptypes.PublicPoolStatusActive,
			OperatorFee:          0,
			MinOperatorShareRate: 0,
			TotalShares:          math.ZeroInt(),
			OperatorShares:       math.ZeroInt(),
			Strategies:           make([]math.Int, perptypes.NbStrategies),
		},
	}))
	// Mark fakeRiskKeeper as risky so the post-mint risk check rejects.
	env.risk.risky = true

	_, err := srv.MintShares(env.ctx, &types.MsgMintShares{
		Sender:           owner,
		PoolAccountIndex: 9001,
		PrincipalAmount:  1_000_000,
	})
	require.ErrorIs(t, err, types.ErrRiskRegression)
}

// TestCreatePublicPool_BlockedByRiskRegression mirrors the above for
// the pool-creation seed transfer (audit H2).
func TestCreatePublicPool_BlockedByRiskRegression(t *testing.T) {
	env := initTestEnv(t)
	srv := accountkeeper.NewMsgServerImpl(env.ak)

	owner := "px1qv9pzxqlyckngw6zf9g9whn9d3eh4qvgsxc8cx"
	const masterIdx uint64 = 8101
	// Master pays initial_total_shares * INITIAL_POOL_SHARE_VALUE *
	// USDC_TO_COLLATERAL = 10 * 1000 * 1_000_000 = 10_000_000_000.
	require.NoError(t, keepertest.SetAccountForTest(env.ctx, env.ak, types.Account{
		AccountIndex: masterIdx,
		OwnerAddress: owner,
		AccountType:  perptypes.MasterAccountType,
		Collateral:   internalAmount(1_000_000_000),
	}))
	env.risk.risky = true

	_, err := srv.CreatePublicPool(env.ctx, &types.MsgCreatePublicPool{
		Sender:               owner,
		MasterAccountIndex:   masterIdx,
		AccountType:          perptypes.PublicPoolAccountType,
		OperatorFee:          0,
		MinOperatorShareRate: 0,
		InitialTotalShares:   10,
	})
	require.ErrorIs(t, err, types.ErrRiskRegression)
}

// TestStateChangeEvents_AccountUpdate verifies that mutating an existing
// Account row through any cohesive mutator (here AddCollateral, which is
// the keeper API used by x/trade / x/funding / x/liquidation when they
// realise PnL or charge fees) fires an EventAccountUpdated{created=false}
// so off-chain indexers can reconstruct the Accounts table from the
// event stream — the whole reason these events were lifted out of the
// msg_server.
func TestStateChangeEvents_AccountUpdate(t *testing.T) {
	env := initTestEnv(t)

	owner := "px1qv9pzxqlyckngw6zf9g9whn9d3eh4qvgsxc8cx"
	const masterIdx uint64 = 7777
	require.NoError(t, keepertest.SetAccountForTest(env.ctx, env.ak, types.Account{
		AccountIndex: masterIdx,
		OwnerAddress: owner,
		AccountType:  perptypes.MasterAccountType,
		Collateral:   math.ZeroInt(),
	}))

	resetEvents(env)
	require.NoError(t, env.ak.AddCollateral(env.ctx, masterIdx, math.NewInt(123)))

	require.Equal(t, 1, countEvents(env, &types.EventAccountUpdated{}),
		"AddCollateral must emit a single EventAccountUpdated")
}

// TestStateChangeEvents_AccountCreate verifies that creating a sub
// account through the cohesive CreateSubAccount API fires an
// EventAccountUpdated{created=true} carrying the freshly-allocated row.
func TestStateChangeEvents_AccountCreate(t *testing.T) {
	env := initTestEnv(t)

	owner := "px1qv9pzxqlyckngw6zf9g9whn9d3eh4qvgsxc8cx"
	master, err := env.ak.EnsureMasterAccount(env.ctx, sdk.MustAccAddressFromBech32(owner))
	require.NoError(t, err)

	resetEvents(env)
	sub, err := env.ak.CreateSubAccount(env.ctx, master)
	require.NoError(t, err)

	require.Equal(t, 1, countEvents(env, &types.EventAccountUpdated{}),
		"CreateSubAccount must emit a single EventAccountUpdated for the new row")
	require.Equal(t, master.AccountIndex, sub.MasterAccountIndex)
}

// TestStateChangeEvents_AccountAsset_LockPath pins the invariant that
// the orderbook lock path (IncreaseLockedBalance, called by x/orderbook
// on every spot order placement) emits an EventAccountAssetUpdated, so
// indexers see every AccountAsset mutation triggered by resting orders.
func TestStateChangeEvents_AccountAsset_LockPath(t *testing.T) {
	env := initTestEnv(t)

	const accIdx uint64 = 6001
	const assetIdx uint32 = perptypes.USDCAssetIndex
	require.NoError(t, env.ak.AccountAssets.Set(env.ctx,
		collections.Join(accIdx, assetIdx),
		types.AccountAsset{
			AccountIndex:  accIdx,
			AssetIndex:    assetIdx,
			Balance:       math.NewInt(1_000),
			LockedBalance: math.ZeroInt(),
		}))

	resetEvents(env)
	require.NoError(t, env.ak.IncreaseLockedBalance(env.ctx, accIdx, assetIdx, math.NewInt(400)))
	require.Equal(t, 1, countEvents(env, &types.EventAccountAssetUpdated{}),
		"IncreaseLockedBalance must emit EventAccountAssetUpdated (orderbook path)")

	resetEvents(env)
	require.NoError(t, env.ak.DecreaseLockedBalance(env.ctx, accIdx, assetIdx, math.NewInt(100)))
	require.Equal(t, 1, countEvents(env, &types.EventAccountAssetUpdated{}),
		"DecreaseLockedBalance must emit EventAccountAssetUpdated (orderbook path)")
}

// TestStateChangeEvents_AccountAsset_TransferPath proves the x/trade
// spot-fill path also surfaces events (one per side of the transfer).
func TestStateChangeEvents_AccountAsset_TransferPath(t *testing.T) {
	env := initTestEnv(t)

	const fromIdx uint64 = 6101
	const toIdx uint64 = 6102
	const assetIdx uint32 = perptypes.USDCAssetIndex
	require.NoError(t, env.ak.AccountAssets.Set(env.ctx,
		collections.Join(fromIdx, assetIdx),
		types.AccountAsset{
			AccountIndex: fromIdx, AssetIndex: assetIdx,
			Balance: math.NewInt(1_000), LockedBalance: math.ZeroInt(),
		}))
	require.NoError(t, env.ak.AccountAssets.Set(env.ctx,
		collections.Join(toIdx, assetIdx),
		types.AccountAsset{
			AccountIndex: toIdx, AssetIndex: assetIdx,
			Balance: math.ZeroInt(), LockedBalance: math.ZeroInt(),
		}))

	resetEvents(env)
	require.NoError(t, env.ak.TransferAccountAssetBalance(
		env.ctx, fromIdx, toIdx, assetIdx, math.NewInt(200), false /* drainLockedFirst */))
	require.Equal(t, 2, countEvents(env, &types.EventAccountAssetUpdated{}),
		"TransferAccountAssetBalance must emit one EventAccountAssetUpdated per side")
}

// TestStateChangeEvents_Position proves that the x/trade / x/funding
// position-update path (UpdatePosition) fires a position event. This
// is the most critical of the three because positions are mutated
// almost exclusively from outside x/account.
func TestStateChangeEvents_Position(t *testing.T) {
	env := initTestEnv(t)

	resetEvents(env)
	_, err := env.ak.UpdatePosition(env.ctx, 7001, 0, func(p *types.AccountPosition) error {
		p.BaseSize = math.NewInt(5)
		p.EntryQuote = math.NewInt(500)
		p.AllocatedMargin = math.NewInt(50)
		return nil
	})
	require.NoError(t, err)
	require.Equal(t, 1, countEvents(env, &types.EventPositionUpdated{}),
		"UpdatePosition must emit a single EventPositionUpdated")
}

// The following tests pin the msg_server defense-in-depth contract:
// every public handler must call msg.ValidateBasic() on entry so that
// callers that bypass the SDK ante (keeper-level tests like the ones in
// this file, governance proposals routed through MsgServiceRouter,
// future cross-module Msg routers) cannot smuggle malformed messages
// past the stateless invariants. Each test constructs a message that
// passes the per-handler state-dependent checks (so any rejection MUST
// come from ValidateBasic) and asserts the handler returns the
// expected stateless error without touching state.

const validOwner = "px1qv9pzxqlyckngw6zf9g9whn9d3eh4qvgsxc8cx"

// TestMsgServer_Deposit_RejectsInvalidRoute proves the Deposit handler
// surfaces ValidateBasic's route-enum check even when called directly
// from the keeper layer.
func TestMsgServer_Deposit_RejectsInvalidRoute(t *testing.T) {
	env := initTestEnv(t)
	srv := accountkeeper.NewMsgServerImpl(env.ak)

	_, err := srv.Deposit(env.ctx, &types.MsgDeposit{
		Sender:     validOwner,
		AssetIndex: perptypes.USDCAssetIndex,
		Amount:     1_000_000,
		RouteType:  99, // out of {RouteTypePerps, RouteTypeSpot}
	})
	require.ErrorIs(t, err, types.ErrInvalidRoute)
}

// TestMsgServer_Withdraw_RejectsZeroAmount proves the Withdraw handler
// runs ValidateBasic before authorization / state lookups so a zero
// amount is rejected without touching the store.
func TestMsgServer_Withdraw_RejectsZeroAmount(t *testing.T) {
	env := initTestEnv(t)
	srv := accountkeeper.NewMsgServerImpl(env.ak)

	_, err := srv.Withdraw(env.ctx, &types.MsgWithdraw{
		Sender:       validOwner,
		AccountIndex: 1234,
		AssetIndex:   perptypes.USDCAssetIndex,
		Amount:       0,
		RouteType:    perptypes.RouteTypePerps,
	})
	require.ErrorIs(t, err, types.ErrAmountTooSmall)
}

// TestMsgServer_Transfer_RejectsSameAccount proves the Transfer
// handler rejects from == to via ValidateBasic before any pool /
// authorization checks run.
func TestMsgServer_Transfer_RejectsSameAccount(t *testing.T) {
	env := initTestEnv(t)
	srv := accountkeeper.NewMsgServerImpl(env.ak)

	_, err := srv.Transfer(env.ctx, &types.MsgTransfer{
		Sender:           validOwner,
		FromAccountIndex: 4242,
		ToAccountIndex:   4242,
		AssetIndex:       perptypes.USDCAssetIndex,
		Amount:           1_000,
	})
	require.ErrorIs(t, err, types.ErrInvalidParams)
}

// TestMsgServer_UpdateMargin_RejectsInvalidAction proves the
// UpdateMargin handler enforces the Action enum guard from
// ValidateBasic at the keeper-call layer.
func TestMsgServer_UpdateMargin_RejectsInvalidAction(t *testing.T) {
	env := initTestEnv(t)
	srv := accountkeeper.NewMsgServerImpl(env.ak)

	_, err := srv.UpdateMargin(env.ctx, &types.MsgUpdateMargin{
		Sender:       validOwner,
		AccountIndex: 9001,
		MarketIndex:  0,
		Action:       99, // not in {AddMargin, RemoveMargin}
		Amount:       math.NewInt(100),
	})
	require.ErrorIs(t, err, types.ErrInvalidMarginAction)
}

// TestMsgServer_UpdateLeverage_RejectsIMFAboveTick proves the
// UpdateLeverage handler enforces the MarginTick upper bound at the
// keeper-call layer (the per-market floor still requires
// MarketKeeper and is exercised by other tests).
func TestMsgServer_UpdateLeverage_RejectsIMFAboveTick(t *testing.T) {
	env := initTestEnv(t)
	srv := accountkeeper.NewMsgServerImpl(env.ak)

	_, err := srv.UpdateLeverage(env.ctx, &types.MsgUpdateLeverage{
		Sender:                   validOwner,
		AccountIndex:             9002,
		MarketIndex:              0,
		NewMarginMode:            perptypes.CrossMargin,
		NewInitialMarginFraction: uint32(perptypes.MarginTick) + 1,
	})
	require.ErrorIs(t, err, types.ErrInvalidParams)
}

// TestMsgServer_CreatePublicPool_RejectsZeroShares proves the
// CreatePublicPool handler enforces InitialTotalShares > 0 from
// ValidateBasic at the keeper-call layer.
func TestMsgServer_CreatePublicPool_RejectsZeroShares(t *testing.T) {
	env := initTestEnv(t)
	srv := accountkeeper.NewMsgServerImpl(env.ak)

	_, err := srv.CreatePublicPool(env.ctx, &types.MsgCreatePublicPool{
		Sender:               validOwner,
		MasterAccountIndex:   8200,
		AccountType:          perptypes.PublicPoolAccountType,
		OperatorFee:          0,
		MinOperatorShareRate: 0,
		InitialTotalShares:   0, // ValidateBasic floor.
	})
	require.ErrorIs(t, err, types.ErrInvalidParams)
}

// TestMsgServer_UpdatePublicPool_RejectsInvalidStatus proves the
// UpdatePublicPool handler enforces the NewStatus enum from
// ValidateBasic at the keeper-call layer.
func TestMsgServer_UpdatePublicPool_RejectsInvalidStatus(t *testing.T) {
	env := initTestEnv(t)
	srv := accountkeeper.NewMsgServerImpl(env.ak)

	_, err := srv.UpdatePublicPool(env.ctx, &types.MsgUpdatePublicPool{
		Sender:                  validOwner,
		PoolAccountIndex:        8300,
		NewStatus:               42,
		NewOperatorFee:          0,
		NewMinOperatorShareRate: 0,
	})
	require.ErrorIs(t, err, types.ErrInvalidPoolUpdate)
}
