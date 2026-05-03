// Package integration hosts cross-module integration tests for PerpDEX L1.
//
// Each test file builds its own light-weight `integration.App` fixture from
// `cosmossdk.io/testutil/integration`. The fixture wires a small set of
// keepers together inside an in-memory multistore so that we can exercise
// keeper-level interactions without spinning up a full ABCI node.
package integration

import (
	"testing"

	"github.com/stretchr/testify/require"

	abci "github.com/cometbft/cometbft/abci/types"
	cmtprototypes "github.com/cometbft/cometbft/proto/tendermint/types"

	"cosmossdk.io/core/appmodule"
	"cosmossdk.io/log"
	"cosmossdk.io/math"
	storetypes "cosmossdk.io/store/types"

	"github.com/cosmos/cosmos-sdk/codec"
	addresscodec "github.com/cosmos/cosmos-sdk/codec/address"
	"github.com/cosmos/cosmos-sdk/runtime"
	"github.com/cosmos/cosmos-sdk/testutil/integration"
	sdk "github.com/cosmos/cosmos-sdk/types"
	moduletestutil "github.com/cosmos/cosmos-sdk/types/module/testutil"
	"github.com/cosmos/cosmos-sdk/x/auth"
	authkeeper "github.com/cosmos/cosmos-sdk/x/auth/keeper"
	authsims "github.com/cosmos/cosmos-sdk/x/auth/simulation"
	authtypes "github.com/cosmos/cosmos-sdk/x/auth/types"
	"github.com/cosmos/cosmos-sdk/x/auth/vesting"
	"github.com/cosmos/cosmos-sdk/x/bank"
	bankkeeper "github.com/cosmos/cosmos-sdk/x/bank/keeper"
	banktypes "github.com/cosmos/cosmos-sdk/x/bank/types"
	"github.com/cosmos/cosmos-sdk/x/distribution"
	distrkeeper "github.com/cosmos/cosmos-sdk/x/distribution/keeper"
	distrtypes "github.com/cosmos/cosmos-sdk/x/distribution/types"
	minttypes "github.com/cosmos/cosmos-sdk/x/mint/types"
	"github.com/cosmos/cosmos-sdk/x/staking"
	stakingkeeper "github.com/cosmos/cosmos-sdk/x/staking/keeper"
	stakingtypes "github.com/cosmos/cosmos-sdk/x/staking/types"

	// Imported for its side-effect: registers the `px` bech32 prefix on the
	// global SDK config so address parsing works the same as in production.
	_ "github.com/perpdex/perpdex-l1/app"
)

// fixture bundles the keepers and context that every integration test in
// this package needs. Tests build it via `initFixture` and then drive the
// in-memory keepers directly.
type fixture struct {
	app *integration.App

	sdkCtx sdk.Context
	cdc    codec.Codec
	keys   map[string]*storetypes.KVStoreKey

	accountKeeper authkeeper.AccountKeeper
	bankKeeper    bankkeeper.Keeper
	distrKeeper   distrkeeper.Keeper
	stakingKeeper *stakingkeeper.Keeper
}

// initFixture wires together a minimal app-like environment: auth, bank,
// staking and distribution. It is enough to exercise delegations, transfers
// and reward bookkeeping; tests that need more modules can extend this in
// place.
func initFixture(tb testing.TB) *fixture {
	tb.Helper()

	keys := storetypes.NewKVStoreKeys(
		authtypes.StoreKey,
		banktypes.StoreKey,
		stakingtypes.StoreKey,
		distrtypes.StoreKey,
	)
	cdc := moduletestutil.MakeTestEncodingConfig(
		auth.AppModuleBasic{},
		bank.AppModuleBasic{},
		staking.AppModuleBasic{},
		distribution.AppModuleBasic{},
		vesting.AppModuleBasic{},
	).Codec

	logger := log.NewTestLogger(tb)
	cms := integration.CreateMultiStore(keys, logger)
	sdkCtx := sdk.NewContext(cms, cmtprototypes.Header{}, true, logger)

	authority := authtypes.NewModuleAddress("gov")

	maccPerms := map[string][]string{
		distrtypes.ModuleName:          nil,
		minttypes.ModuleName:           {authtypes.Minter},
		stakingtypes.BondedPoolName:    {authtypes.Burner, authtypes.Staking},
		stakingtypes.NotBondedPoolName: {authtypes.Burner, authtypes.Staking},
	}

	accountKeeper := authkeeper.NewAccountKeeper(
		cdc,
		runtime.NewKVStoreService(keys[authtypes.StoreKey]),
		authtypes.ProtoBaseAccount,
		maccPerms,
		addresscodec.NewBech32Codec(sdk.GetConfig().GetBech32AccountAddrPrefix()),
		sdk.GetConfig().GetBech32AccountAddrPrefix(),
		authority.String(),
	)

	blockedAddresses := map[string]bool{
		accountKeeper.GetAuthority(): false,
	}
	bankKeeper := bankkeeper.NewBaseKeeper(
		cdc,
		runtime.NewKVStoreService(keys[banktypes.StoreKey]),
		accountKeeper,
		blockedAddresses,
		authority.String(),
		log.NewNopLogger(),
	)

	stakingKeeper := stakingkeeper.NewKeeper(
		cdc,
		runtime.NewKVStoreService(keys[stakingtypes.StoreKey]),
		accountKeeper,
		bankKeeper,
		authority.String(),
		addresscodec.NewBech32Codec(sdk.GetConfig().GetBech32ValidatorAddrPrefix()),
		addresscodec.NewBech32Codec(sdk.GetConfig().GetBech32ConsensusAddrPrefix()),
	)
	distrKeeper := distrkeeper.NewKeeper(
		cdc,
		runtime.NewKVStoreService(keys[distrtypes.StoreKey]),
		accountKeeper,
		bankKeeper,
		stakingKeeper,
		distrtypes.ModuleName,
		authority.String(),
	)

	authModule := auth.NewAppModule(cdc, accountKeeper, authsims.RandomGenesisAccounts, nil)
	bankModule := bank.NewAppModule(cdc, bankKeeper, accountKeeper, nil)
	stakingModule := staking.NewAppModule(cdc, stakingKeeper, accountKeeper, bankKeeper, nil)
	distrModule := distribution.NewAppModule(cdc, distrKeeper, accountKeeper, bankKeeper, stakingKeeper, nil)

	integrationApp := integration.NewIntegrationApp(sdkCtx, logger, keys, cdc, map[string]appmodule.AppModule{
		authtypes.ModuleName:    authModule,
		banktypes.ModuleName:    bankModule,
		stakingtypes.ModuleName: stakingModule,
		distrtypes.ModuleName:   distrModule,
	})

	sdkCtx = sdk.UnwrapSDKContext(integrationApp.Context())

	stakingKeeper.SetHooks(stakingtypes.NewMultiStakingHooks(
		distrKeeper.Hooks(),
	))

	stakingtypes.RegisterMsgServer(integrationApp.MsgServiceRouter(), stakingkeeper.NewMsgServerImpl(stakingKeeper))
	stakingtypes.RegisterQueryServer(integrationApp.QueryHelper(), stakingkeeper.NewQuerier(stakingKeeper))
	banktypes.RegisterMsgServer(integrationApp.MsgServiceRouter(), bankkeeper.NewMsgServerImpl(bankKeeper))
	banktypes.RegisterQueryServer(integrationApp.QueryHelper(), bankkeeper.NewQuerier(&bankKeeper))

	require.NoError(tb, stakingKeeper.SetParams(sdkCtx, stakingtypes.DefaultParams()))

	return &fixture{
		app:           integrationApp,
		sdkCtx:        sdkCtx,
		cdc:           cdc,
		keys:          keys,
		accountKeeper: accountKeeper,
		bankKeeper:    bankKeeper,
		distrKeeper:   distrKeeper,
		stakingKeeper: stakingKeeper,
	}
}

// delegateCoinsFromAccount is a small wrapper around stakingKeeper.Delegate
// to keep tests terse.
func delegateCoinsFromAccount(
	ctx sdk.Context,
	sk *stakingkeeper.Keeper,
	addr sdk.AccAddress,
	amount math.Int,
	val stakingtypes.ValidatorI,
) error {
	_, err := sk.Delegate(ctx, addr, amount, stakingtypes.Unbonded, val.(stakingtypes.Validator), true)
	return err
}

// applyValidatorSetUpdates flushes pending validator set changes and
// (optionally) asserts the resulting count.
func applyValidatorSetUpdates(
	t *testing.T,
	ctx sdk.Context,
	k *stakingkeeper.Keeper,
	expectedUpdatesLen int,
) []abci.ValidatorUpdate {
	t.Helper()
	updates, err := k.ApplyAndReturnValidatorSetUpdates(ctx)
	require.NoError(t, err)
	if expectedUpdatesLen >= 0 {
		require.Equal(t, expectedUpdatesLen, len(updates), "%v", updates)
	}
	return updates
}
