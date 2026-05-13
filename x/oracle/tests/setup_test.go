// Package tests is the external test package that exercises every public
// surface of x/oracle (abci/codec, abci/ve, daemon, keeper) from a single
// import root.
//
// All sub-package tests previously lived next to the production code in
// four separate `_test` packages (`codec_test`, `ve_test`, `daemon_test`,
// `keeper_test`). Consolidating them under `package tests` keeps the
// fixtures next to the suites that actually need them and makes it
// obvious that the only allowed entry points are the exported APIs.
//
// setup_test.go hosts the cross-suite fixtures: keeper construction and
// the VE handler bundle. Suite-local fakes (the gRPC fake sidecar and
// the resolver's fake market/asset readers) stay inside the file that
// uses them.
package tests

import (
	"testing"
	"time"

	cmtprototypes "github.com/cometbft/cometbft/proto/tendermint/types"
	"github.com/stretchr/testify/require"

	"cosmossdk.io/log"
	storetypes "cosmossdk.io/store/types"

	"github.com/cosmos/cosmos-sdk/runtime"
	"github.com/cosmos/cosmos-sdk/testutil/integration"
	sdk "github.com/cosmos/cosmos-sdk/types"
	moduletestutil "github.com/cosmos/cosmos-sdk/types/module/testutil"

	oracle "github.com/perpdex/perpdex-l1/x/oracle"
	oraclecodec "github.com/perpdex/perpdex-l1/x/oracle/abci/codec"
	oraclekeeper "github.com/perpdex/perpdex-l1/x/oracle/keeper"
	oracletypes "github.com/perpdex/perpdex-l1/x/oracle/types"
)

// govAddrFixture is the bech32 used for the gov authority across all
// vote-extension tests. We compute it on first access so the value is
// guaranteed to round-trip through `sdk.AccAddressFromBech32` regardless
// of the prefix the surrounding test environment has installed on
// `sdk.GetConfig`.
var govAddrFixture = func() string {
	return sdk.AccAddress(make([]byte, 20)).String()
}()

// newOracleKeeper builds an isolated oracle keeper backed by an
// in-memory multistore and a fixed block time. It is the minimal
// fixture used by suites that exercise the keeper directly (price get /
// set / freshness checks) without bringing in the VE codec stack.
func newOracleKeeper(t *testing.T) (oraclekeeper.Keeper, sdk.Context) {
	t.Helper()
	keys := storetypes.NewKVStoreKeys(oracletypes.StoreKey)
	cdc := moduletestutil.MakeTestEncodingConfig().Codec
	cms := integration.CreateMultiStore(keys, log.NewTestLogger(t))
	ctx := sdk.NewContext(cms, cmtprototypes.Header{Time: time.Unix(1_700_000_000, 0)}, true, log.NewTestLogger(t))
	k := oraclekeeper.NewKeeper(
		cdc,
		runtime.NewKVStoreService(keys[oracletypes.StoreKey]),
		"auth",
	)
	require.NoError(t, k.Params.Set(ctx, oracletypes.DefaultParams()))
	return k, ctx
}

// newVEFixture wires up the full vote-extension pipeline: a keeper, the
// raw VE/EC codecs, and a freshly constructed handler. The returned
// context has the cometbft consensus params already configured to
// enable vote extensions from height 1 — without that, ProcessProposal
// would skip the VE checks the suite is meant to assert on.
func newVEFixture(t *testing.T) (oraclekeeper.Keeper, sdk.Context, *oraclekeeper.VoteExtensionHandler, oraclecodec.VoteExtensionCodec, oraclecodec.ExtendedCommitCodec) {
	t.Helper()
	keys := storetypes.NewKVStoreKeys(oracletypes.StoreKey)
	encCfg := moduletestutil.MakeTestEncodingConfig(oracle.AppModuleBasic{})
	cdc := encCfg.Codec
	cms := integration.CreateMultiStore(keys, log.NewTestLogger(t))
	ctx := sdk.NewContext(cms,
		cmtprototypes.Header{Time: time.Unix(1_700_000_000, 0)},
		false, log.NewTestLogger(t),
	).WithConsensusParams(cmtprototypes.ConsensusParams{
		Abci: &cmtprototypes.ABCIParams{VoteExtensionsEnableHeight: 1},
	})
	k := oraclekeeper.NewKeeper(cdc,
		runtime.NewKVStoreService(keys[oracletypes.StoreKey]),
		govAddrFixture,
	)
	require.NoError(t, k.Params.Set(ctx, oracletypes.DefaultParams()))
	veCodec := oraclecodec.NewRawVoteExtensionCodec()
	ecCodec := oraclecodec.NewRawExtendedCommitCodec()
	h := oraclekeeper.NewVoteExtensionHandler(k, veCodec, ecCodec)
	return k, ctx, h, veCodec, ecCodec
}
