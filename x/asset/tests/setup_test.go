// Package tests collects the integration-style coverage for the
// x/asset module. Files in this package are organised by business
// surface (registration, updates, queries, genesis) rather than by
// source file, so each scenario lives next to siblings that exercise
// the same behaviour.
//
// setup_test.go holds the shared fixtures: the bech32 test addresses,
// the testEnv harness (context + keeper + wired servers), and the
// minimal struct builders that individual tests mutate. Everything in
// this file is package-private to the tests package.
package tests

import (
	"testing"

	"github.com/stretchr/testify/require"

	cmtprototypes "github.com/cometbft/cometbft/proto/tendermint/types"

	"cosmossdk.io/log"
	storetypes "cosmossdk.io/store/types"

	"github.com/cosmos/cosmos-sdk/runtime"
	"github.com/cosmos/cosmos-sdk/testutil/integration"
	sdk "github.com/cosmos/cosmos-sdk/types"
	moduletestutil "github.com/cosmos/cosmos-sdk/types/module/testutil"

	perptypes "github.com/perpdex/perpdex-l1/types"
	assetkeeper "github.com/perpdex/perpdex-l1/x/asset/keeper"
	"github.com/perpdex/perpdex-l1/x/asset/types"
)

const (
	testAuthority = "px1xqcnyve5x5mrwwpev93xxer9venks6t29ke4l8"
	// otherAddr is a valid but unrelated bech32 used as a stand-in for
	// a non-governance signer in authority-rejection tests.
	otherAddr = "px1qv9pzxqlyckngw6zf9g9whn9d3eh4qvgsxc8cx"
)

// testEnv bundles the dependencies every keeper test needs.
type testEnv struct {
	ctx    sdk.Context
	keeper assetkeeper.Keeper
	srv    types.MsgServer
	q      types.QueryServer
}

func newTestEnv(t *testing.T) *testEnv {
	t.Helper()
	env := newEmptyTestEnv(t)
	require.NoError(t, env.keeper.InitGenesis(env.ctx, *types.DefaultGenesis()))
	return env
}

// newEmptyTestEnv builds a keeper with an empty store — no InitGenesis
// has been run yet. Used by tests that exercise InitGenesis itself
// (e.g. the export → import round-trip).
func newEmptyTestEnv(t *testing.T) *testEnv {
	t.Helper()

	cfg := sdk.GetConfig()
	cfg.SetBech32PrefixForAccount("px", "pxpub")

	keys := storetypes.NewKVStoreKeys(types.StoreKey)
	cdc := moduletestutil.MakeTestEncodingConfig().Codec
	logger := log.NewTestLogger(t)
	cms := integration.CreateMultiStore(keys, logger)
	ctx := sdk.NewContext(cms, cmtprototypes.Header{}, true, logger)

	k := assetkeeper.NewKeeper(
		cdc,
		runtime.NewKVStoreService(keys[types.StoreKey]),
		testAuthority,
	)
	return &testEnv{
		ctx:    ctx,
		keeper: k,
		srv:    assetkeeper.NewMsgServerImpl(k),
		q:      assetkeeper.NewQuerier(k),
	}
}

// validRegisterMsg returns a MsgRegisterAsset that passes every shape
// check. Tests mutate one field at a time to exercise the rejection
// paths without copy-pasting the whole struct.
func validRegisterMsg() *types.MsgRegisterAsset {
	return &types.MsgRegisterAsset{
		Authority:           testAuthority,
		Denom:               "ubtc",
		DisplayName:         "BTC",
		Decimals:            8,
		ExtensionMultiplier: 1,
		MinTransferAmount:   1,
		MinWithdrawalAmount: 1,
		MarginMode:          perptypes.MarginModeDisabled,
	}
}

// sampleAsset builds a minimal asset record for the keeper-level
// CreateAsset / UpdateAsset tests; tests override fields they care
// about.
func sampleAsset(idx uint32, denom, name string) types.Asset {
	return types.Asset{
		AssetIndex:          idx,
		Denom:               denom,
		DisplayName:         name,
		Decimals:            8,
		ExtensionMultiplier: 1,
		MinTransferAmount:   1,
		MinWithdrawalAmount: 1,
		MarginMode:          perptypes.MarginModeDisabled,
		Enabled:             true,
	}
}

// validBTC returns a well-formed non-margin asset suitable for
// concatenating onto DefaultGenesis().Assets without tripping the
// per-row validator. Tests override individual fields to exercise
// specific failure modes.
func validBTC() types.Asset {
	return types.Asset{
		AssetIndex:          4,
		Denom:               "ubtc",
		DisplayName:         "BTC",
		Decimals:            8,
		ExtensionMultiplier: 1,
		MinTransferAmount:   1,
		MinWithdrawalAmount: 1,
		MarginMode:          perptypes.MarginModeDisabled,
		Enabled:             true,
	}
}
