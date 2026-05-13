// Shared fixtures for the x/account test suite: an in-memory keeper
// wired against minimal fake bank / risk / market dependencies, plus
// the event helpers used by the event-emission contract tests.
//
// initTestEnv produces a fresh testEnv per test so suites can mutate
// the fake-risk flag (and the embedded fakeMarketKeeper) without
// cross-test contamination. fakeBankKeeper is a no-op stub because
// the keeper-level tests never actually move coins through the bank
// module.
package tests

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"

	cmtprototypes "github.com/cometbft/cometbft/proto/tendermint/types"

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
	"github.com/perpdex/perpdex-l1/x/account/types"
	assetkeeper "github.com/perpdex/perpdex-l1/x/asset/keeper"
	assettypes "github.com/perpdex/perpdex-l1/x/asset/types"
	markettypes "github.com/perpdex/perpdex-l1/x/market/types"
	risktypes "github.com/perpdex/perpdex-l1/x/risk/types"
)

// validOwner is the canonical px1... bech32 address used by every
// fixture that needs an authorised sender. It is shared between the
// keeper-level scenario tests and the types-level msg ValidateBasic
// tests so both layers exercise the same address format.
const validOwner = "px1qv9pzxqlyckngw6zf9g9whn9d3eh4qvgsxc8cx"

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

// internalAmount converts an external USDC base amount (6 decimals) to the
// internal collateral-precision delta used by spot AccountAsset rows.
func internalAmount(externalAmount uint64) math.Int {
	return math.NewIntFromUint64(externalAmount).
		Mul(math.NewIntFromUint64(perptypes.USDCToCollateralMultiplier))
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
