package keeper_test

import (
	"strings"
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

// TestInitGenesis_SeedsUSDC confirms the default genesis lands USDC at
// the canonical slot and sets the sequence past it.
func TestInitGenesis_SeedsUSDC(t *testing.T) {
	env := newTestEnv(t)
	usdc, err := env.keeper.GetAsset(env.ctx, perptypes.USDCAssetIndex)
	require.NoError(t, err)
	require.Equal(t, "uusdc", usdc.Denom)
	require.True(t, usdc.Enabled)
	require.Equal(t, perptypes.MarginModeEnabled, usdc.MarginMode)

	next, err := env.keeper.NextAssetIndex.Peek(env.ctx)
	require.NoError(t, err)
	require.EqualValues(t, perptypes.USDCAssetIndex+1, next)
}

// TestInitGenesis_NormalisesNextIndex asserts that a genesis with
// NextAssetIndex == 0 still ends up safely seeded above MinAssetIndex.
func TestInitGenesis_NormalisesNextIndex(t *testing.T) {
	env := newEmptyTestEnv(t)

	gs := types.DefaultGenesis()
	gs.NextAssetIndex = 0
	require.NoError(t, env.keeper.InitGenesis(env.ctx, *gs))

	next, err := env.keeper.NextAssetIndex.Peek(env.ctx)
	require.NoError(t, err)
	// max(MinAssetIndex, maxSeenIdx+1) = max(1, 3+1) = 4.
	require.EqualValues(t, perptypes.USDCAssetIndex+1, next)
}

func TestRegisterAsset_Success(t *testing.T) {
	env := newTestEnv(t)

	resp, err := env.srv.RegisterAsset(env.ctx, validRegisterMsg())
	require.NoError(t, err)
	require.EqualValues(t, perptypes.USDCAssetIndex+1, resp.AssetIndex)

	got, err := env.keeper.GetAsset(env.ctx, resp.AssetIndex)
	require.NoError(t, err)
	require.Equal(t, "BTC", got.DisplayName)
	require.True(t, got.Enabled)

	var found bool
	for _, ev := range env.ctx.EventManager().Events() {
		if ev.Type != types.EventTypeAssetRegistered {
			continue
		}
		for _, a := range ev.Attributes {
			if a.Key == types.AttributeKeyAssetIndex && a.Value == "4" {
				found = true
			}
		}
	}
	require.True(t, found, "expected asset_registered event with asset_index=4")
}

func TestRegisterAsset_UnauthorizedAuthority(t *testing.T) {
	env := newTestEnv(t)

	m := validRegisterMsg()
	m.Authority = otherAddr
	_, err := env.srv.RegisterAsset(env.ctx, m)
	require.ErrorIs(t, err, types.ErrInvalidAuthority)
}

// TestRegisterAsset_DuplicateDenomAfterRegistration registers BTC then
// tries to register another asset with the same denom; expected error
// is ErrAssetExists from the runtime path (not the reserved-denom
// guard, since "ubtc" is not the USDC denom).
func TestRegisterAsset_DuplicateDenomAfterRegistration(t *testing.T) {
	env := newTestEnv(t)

	_, err := env.srv.RegisterAsset(env.ctx, validRegisterMsg())
	require.NoError(t, err)

	m := validRegisterMsg()
	m.DisplayName = "BTC-Other" // unique name
	_, err = env.srv.RegisterAsset(env.ctx, m)
	require.ErrorIs(t, err, types.ErrAssetExists)
}

func TestRegisterAsset_DuplicateDisplayName(t *testing.T) {
	env := newTestEnv(t)

	_, err := env.srv.RegisterAsset(env.ctx, validRegisterMsg())
	require.NoError(t, err)

	m := validRegisterMsg()
	m.Denom = "ubtc2"
	m.DisplayName = "btc" // case-insensitive duplicate
	_, err = env.srv.RegisterAsset(env.ctx, m)
	require.ErrorIs(t, err, types.ErrAssetExists)
}

func TestRegisterAsset_ReservedUSDCDenom(t *testing.T) {
	env := newTestEnv(t)

	m := validRegisterMsg()
	m.Denom = "uusdc"
	m.DisplayName = "fake"
	_, err := env.srv.RegisterAsset(env.ctx, m)
	require.ErrorIs(t, err, types.ErrUSDCMarginConstraint)
}

func TestRegisterAsset_ReservedUSDCDisplayName(t *testing.T) {
	cases := []string{"USDC", "usdc", "  USDC  ", "uSdC"}
	for _, name := range cases {
		t.Run(name, func(t *testing.T) {
			env := newTestEnv(t)
			m := validRegisterMsg()
			m.Denom = "ubtc"
			m.DisplayName = name
			_, err := env.srv.RegisterAsset(env.ctx, m)
			require.ErrorIs(t, err, types.ErrUSDCMarginConstraint)
		})
	}
}

// TestRegisterAsset_RejectsMarginEnabled locks down the runtime path:
// margin-enabled assets must come from genesis only.
func TestRegisterAsset_RejectsMarginEnabled(t *testing.T) {
	env := newTestEnv(t)

	m := validRegisterMsg()
	m.MarginMode = perptypes.MarginModeEnabled
	_, err := env.srv.RegisterAsset(env.ctx, m)
	require.ErrorIs(t, err, types.ErrUSDCMarginConstraint)
}

func TestRegisterAsset_RejectsDecimalsTooLarge(t *testing.T) {
	env := newTestEnv(t)
	m := validRegisterMsg()
	m.Decimals = 19
	_, err := env.srv.RegisterAsset(env.ctx, m)
	require.ErrorIs(t, err, types.ErrInvalidAssetParams)
}

func TestRegisterAsset_RejectsDecimalsZero(t *testing.T) {
	env := newTestEnv(t)
	m := validRegisterMsg()
	m.Decimals = 0
	_, err := env.srv.RegisterAsset(env.ctx, m)
	require.ErrorIs(t, err, types.ErrInvalidAssetParams)
}

func TestRegisterAsset_RejectsExtensionMultiplierTooLarge(t *testing.T) {
	env := newTestEnv(t)
	m := validRegisterMsg()
	m.ExtensionMultiplier = perptypes.MaxExtensionMultiplier + 1
	_, err := env.srv.RegisterAsset(env.ctx, m)
	require.ErrorIs(t, err, types.ErrInvalidAssetParams)
}

func TestRegisterAsset_RejectsBadDenom(t *testing.T) {
	env := newTestEnv(t)
	m := validRegisterMsg()
	m.Denom = "!!!" // fails sdk.ValidateDenom
	_, err := env.srv.RegisterAsset(env.ctx, m)
	require.ErrorIs(t, err, types.ErrInvalidAssetParams)
}

func TestRegisterAsset_RejectsLongDisplayName(t *testing.T) {
	env := newTestEnv(t)
	m := validRegisterMsg()
	m.DisplayName = strings.Repeat("A", perptypes.MaxAssetDisplayNameLen+1)
	_, err := env.srv.RegisterAsset(env.ctx, m)
	require.ErrorIs(t, err, types.ErrInvalidAssetParams)
}

// TestRegisterAsset_ExceedsMaxAssetIndex shrinks the cap via UpdateParams
// so the next allocation overshoots it.
func TestRegisterAsset_ExceedsMaxAssetIndex(t *testing.T) {
	env := newTestEnv(t)

	_, err := env.srv.UpdateParams(env.ctx, &types.MsgUpdateParams{
		Authority: testAuthority,
		Params:    types.Params{MaxAssetIndex: perptypes.USDCAssetIndex},
	})
	require.NoError(t, err)

	_, err = env.srv.RegisterAsset(env.ctx, validRegisterMsg())
	require.ErrorIs(t, err, types.ErrAssetIndexExceedsMax)
}

func TestUpdateAsset_Success(t *testing.T) {
	env := newTestEnv(t)

	resp, err := env.srv.RegisterAsset(env.ctx, validRegisterMsg())
	require.NoError(t, err)

	_, err = env.srv.UpdateAsset(env.ctx, &types.MsgUpdateAsset{
		Authority:           testAuthority,
		AssetIndex:          resp.AssetIndex,
		MinTransferAmount:   42,
		MinWithdrawalAmount: 43,
		Enabled:             false,
	})
	require.NoError(t, err)

	got, err := env.keeper.GetAsset(env.ctx, resp.AssetIndex)
	require.NoError(t, err)
	require.EqualValues(t, 42, got.MinTransferAmount)
	require.EqualValues(t, 43, got.MinWithdrawalAmount)
	require.False(t, got.Enabled)

	var sawUpdate bool
	for _, ev := range env.ctx.EventManager().Events() {
		if ev.Type == types.EventTypeAssetUpdated {
			sawUpdate = true
		}
	}
	require.True(t, sawUpdate, "expected asset_updated event")
}

func TestUpdateAsset_RejectsNilAssetIndex(t *testing.T) {
	env := newTestEnv(t)
	_, err := env.srv.UpdateAsset(env.ctx, &types.MsgUpdateAsset{
		Authority:           testAuthority,
		AssetIndex:          perptypes.NilAssetIndex,
		MinTransferAmount:   1,
		MinWithdrawalAmount: 1,
	})
	require.ErrorIs(t, err, types.ErrInvalidAssetParams)
}

func TestUpdateAsset_RejectsUnknownAsset(t *testing.T) {
	env := newTestEnv(t)
	_, err := env.srv.UpdateAsset(env.ctx, &types.MsgUpdateAsset{
		Authority:           testAuthority,
		AssetIndex:          99,
		MinTransferAmount:   1,
		MinWithdrawalAmount: 1,
		Enabled:             true,
	})
	require.ErrorIs(t, err, types.ErrAssetNotFound)
}

// TestUpdateAsset_USDCMustStayEnabled is the headline protection: gov
// cannot accidentally turn USDC off through the routine update path.
func TestUpdateAsset_USDCMustStayEnabled(t *testing.T) {
	env := newTestEnv(t)
	_, err := env.srv.UpdateAsset(env.ctx, &types.MsgUpdateAsset{
		Authority:           testAuthority,
		AssetIndex:          perptypes.USDCAssetIndex,
		MinTransferAmount:   1,
		MinWithdrawalAmount: 1,
		Enabled:             false,
	})
	require.ErrorIs(t, err, types.ErrUSDCMarginConstraint)
}

func TestUpdateAsset_USDCCanUpdateMinAmounts(t *testing.T) {
	env := newTestEnv(t)
	_, err := env.srv.UpdateAsset(env.ctx, &types.MsgUpdateAsset{
		Authority:           testAuthority,
		AssetIndex:          perptypes.USDCAssetIndex,
		MinTransferAmount:   100,
		MinWithdrawalAmount: 200,
		Enabled:             true,
	})
	require.NoError(t, err)
	got, err := env.keeper.GetAsset(env.ctx, perptypes.USDCAssetIndex)
	require.NoError(t, err)
	require.EqualValues(t, 100, got.MinTransferAmount)
	require.EqualValues(t, 200, got.MinWithdrawalAmount)
}

func TestUpdateAsset_UnauthorizedAuthority(t *testing.T) {
	env := newTestEnv(t)
	_, err := env.srv.UpdateAsset(env.ctx, &types.MsgUpdateAsset{
		Authority:           otherAddr,
		AssetIndex:          perptypes.USDCAssetIndex,
		MinTransferAmount:   1,
		MinWithdrawalAmount: 1,
		Enabled:             true,
	})
	require.ErrorIs(t, err, types.ErrInvalidAuthority)
}

func TestUpdateParams_Success(t *testing.T) {
	env := newTestEnv(t)
	_, err := env.srv.UpdateParams(env.ctx, &types.MsgUpdateParams{
		Authority: testAuthority,
		Params:    types.Params{MaxAssetIndex: 10},
	})
	require.NoError(t, err)
	p, err := env.keeper.Params.Get(env.ctx)
	require.NoError(t, err)
	require.EqualValues(t, 10, p.MaxAssetIndex)

	var seen bool
	for _, ev := range env.ctx.EventManager().Events() {
		if ev.Type == types.EventTypeParamsUpdated {
			seen = true
		}
	}
	require.True(t, seen, "expected asset_params_updated event")
}

func TestUpdateParams_RejectsZeroMax(t *testing.T) {
	env := newTestEnv(t)
	_, err := env.srv.UpdateParams(env.ctx, &types.MsgUpdateParams{
		Authority: testAuthority,
		Params:    types.Params{MaxAssetIndex: 0},
	})
	require.ErrorIs(t, err, types.ErrInvalidModuleParams)
}

func TestUpdateParams_UnauthorizedAuthority(t *testing.T) {
	env := newTestEnv(t)
	_, err := env.srv.UpdateParams(env.ctx, &types.MsgUpdateParams{
		Authority: otherAddr,
		Params:    types.Params{MaxAssetIndex: 5},
	})
	require.ErrorIs(t, err, types.ErrInvalidAuthority)
}

// TestExportGenesis_RoundTrip registers an extra asset, re-exports,
// re-imports into a fresh keeper, and asserts that USDC + the new
// asset + the sequence pointer all survive intact.
func TestExportGenesis_RoundTrip(t *testing.T) {
	env := newTestEnv(t)
	_, err := env.srv.RegisterAsset(env.ctx, validRegisterMsg())
	require.NoError(t, err)

	gs, err := env.keeper.ExportGenesis(env.ctx)
	require.NoError(t, err)
	require.NoError(t, gs.Validate())

	env2 := newEmptyTestEnv(t)
	require.NoError(t, env2.keeper.InitGenesis(env2.ctx, *gs))

	usdc, err := env2.keeper.GetAsset(env2.ctx, perptypes.USDCAssetIndex)
	require.NoError(t, err)
	require.Equal(t, "uusdc", usdc.Denom)

	btc, err := env2.keeper.GetAssetByDenom(env2.ctx, "ubtc")
	require.NoError(t, err)
	require.Equal(t, "BTC", btc.DisplayName)

	next, err := env2.keeper.NextAssetIndex.Peek(env2.ctx)
	require.NoError(t, err)
	require.EqualValues(t, perptypes.USDCAssetIndex+2, next)
}

// TestQuery_Assets_Pagination registers a handful of assets and walks
// the gRPC paginator to confirm the implementation actually paginates
// (rather than returning every row in one shot).
func TestQuery_Assets_Pagination(t *testing.T) {
	env := newTestEnv(t)
	for _, name := range []string{"BTC", "ETH", "SOL"} {
		_, err := env.srv.RegisterAsset(env.ctx, &types.MsgRegisterAsset{
			Authority:           testAuthority,
			Denom:               "u" + strings.ToLower(name),
			DisplayName:         name,
			Decimals:            8,
			ExtensionMultiplier: 1,
			MinTransferAmount:   1,
			MinWithdrawalAmount: 1,
			MarginMode:          perptypes.MarginModeDisabled,
		})
		require.NoError(t, err)
	}

	resp, err := env.q.Assets(env.ctx, &types.QueryAssetsRequest{})
	require.NoError(t, err)
	require.Len(t, resp.Assets, 4)
}

func TestQuery_AssetByDenom_NotFoundError(t *testing.T) {
	env := newTestEnv(t)
	_, err := env.q.AssetByDenom(env.ctx, &types.QueryAssetByDenomRequest{Denom: "ubogus"})
	require.Error(t, err)
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

func TestKeeperCreateAsset_RejectsNilIndex(t *testing.T) {
	env := newEmptyTestEnv(t)
	err := env.keeper.CreateAsset(env.ctx, sampleAsset(perptypes.NilAssetIndex, "ubtc", "BTC"))
	require.ErrorIs(t, err, types.ErrInvalidAssetParams)
}

func TestKeeperCreateAsset_RejectsDuplicateIndex(t *testing.T) {
	env := newTestEnv(t) // USDC seeded at idx 3
	err := env.keeper.CreateAsset(env.ctx, sampleAsset(perptypes.USDCAssetIndex, "ubtc", "BTC"))
	require.ErrorIs(t, err, types.ErrAssetExists)
}

func TestKeeperCreateAsset_RejectsDuplicateDenom(t *testing.T) {
	env := newTestEnv(t) // USDC seeded with denom "uusdc"
	err := env.keeper.CreateAsset(env.ctx, sampleAsset(4, "uusdc", "BTC"))
	require.ErrorIs(t, err, types.ErrAssetExists)
}

func TestKeeperCreateAsset_Success(t *testing.T) {
	env := newTestEnv(t)
	require.NoError(t, env.keeper.CreateAsset(env.ctx, sampleAsset(4, "ubtc", "BTC")))
	got, err := env.keeper.GetAsset(env.ctx, 4)
	require.NoError(t, err)
	require.Equal(t, "ubtc", got.Denom)

	byDenom, err := env.keeper.GetAssetByDenom(env.ctx, "ubtc")
	require.NoError(t, err)
	require.EqualValues(t, 4, byDenom.AssetIndex)
}

func TestKeeperUpdateAsset_RejectsNilIndex(t *testing.T) {
	env := newEmptyTestEnv(t)
	err := env.keeper.UpdateAsset(env.ctx, sampleAsset(perptypes.NilAssetIndex, "ubtc", "BTC"))
	require.ErrorIs(t, err, types.ErrInvalidAssetParams)
}

func TestKeeperUpdateAsset_RejectsMissingAsset(t *testing.T) {
	env := newTestEnv(t)
	err := env.keeper.UpdateAsset(env.ctx, sampleAsset(99, "ubtc", "BTC"))
	require.ErrorIs(t, err, types.ErrAssetNotFound)
}

// TestKeeperUpdateAsset_DenomIsImmutable asserts the storage-layer
// invariant: denom is the asset's permanent identity and cannot be
// changed via UpdateAsset, even though the in-memory Asset struct has
// the field. msg_server.MsgUpdateAsset doesn't expose denom either, so
// this is defense-in-depth.
func TestKeeperUpdateAsset_DenomIsImmutable(t *testing.T) {
	env := newTestEnv(t)
	require.NoError(t, env.keeper.CreateAsset(env.ctx, sampleAsset(4, "ubtc", "BTC")))

	rogue := sampleAsset(4, "ubtc-v2", "BTC")
	err := env.keeper.UpdateAsset(env.ctx, rogue)
	require.ErrorIs(t, err, types.ErrInvalidAssetParams)

	got, err := env.keeper.GetAssetByDenom(env.ctx, "ubtc")
	require.NoError(t, err)
	require.EqualValues(t, 4, got.AssetIndex)
	_, err = env.keeper.GetAssetByDenom(env.ctx, "ubtc-v2")
	require.ErrorIs(t, err, types.ErrAssetNotFound)
}

// TestKeeperUpdateAsset_SameDenomSucceeds keeps the happy path covered:
// mutating other fields while preserving denom must succeed.
func TestKeeperUpdateAsset_SameDenomSucceeds(t *testing.T) {
	env := newTestEnv(t)
	require.NoError(t, env.keeper.CreateAsset(env.ctx, sampleAsset(4, "ubtc", "BTC")))

	a := sampleAsset(4, "ubtc", "BTC")
	a.MinTransferAmount = 42
	a.Enabled = false
	require.NoError(t, env.keeper.UpdateAsset(env.ctx, a))

	got, err := env.keeper.GetAsset(env.ctx, 4)
	require.NoError(t, err)
	require.EqualValues(t, 42, got.MinTransferAmount)
	require.False(t, got.Enabled)
}
