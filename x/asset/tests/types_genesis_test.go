// types_genesis_test.go covers the static GenesisState.Validate
// checks in x/asset/types — the rules that run before the keeper
// ever touches state.
//
// The cases cluster around four invariants:
//
//   - Uniqueness: no duplicate asset_index, no duplicate denom, and
//     no case-insensitive duplicate display_name.
//   - Per-row shape: empty denom, invalid margin mode, out-of-range
//     decimals / extension_multiplier, and oversized display_name are
//     rejected with ErrInvalidAssetParams.
//   - Sequence pointer: NextAssetIndex must sit above every seeded
//     asset_index, with the zero value tolerated and normalised by
//     the keeper.
//   - USDC binding: anything that *looks* like USDC (denom or display
//     name) but isn't at the canonical index — or the canonical row
//     with margin disabled — is rejected with ErrUSDCMarginConstraint.
package tests

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	perptypes "github.com/perpdex/perpdex-l1/types"
	"github.com/perpdex/perpdex-l1/x/asset/types"
)

func TestGenesisValidate_OK(t *testing.T) {
	gs := types.DefaultGenesis()
	require.NoError(t, gs.Validate())
}

func TestGenesisValidate_DuplicateIndex(t *testing.T) {
	gs := types.DefaultGenesis()
	clash := validBTC()
	clash.AssetIndex = gs.Assets[0].AssetIndex
	clash.Denom = "uother"
	clash.DisplayName = "OTHER"
	gs.Assets = append(gs.Assets, clash)
	require.ErrorIs(t, gs.Validate(), types.ErrAssetExists)
}

func TestGenesisValidate_DuplicateDenom(t *testing.T) {
	gs := types.DefaultGenesis()
	clash := validBTC()
	clash.Denom = gs.Assets[0].Denom
	clash.DisplayName = "OTHER"
	gs.Assets = append(gs.Assets, clash)
	require.ErrorIs(t, gs.Validate(), types.ErrAssetExists)
}

// Case-insensitive duplicate display_name must be rejected so
// look-alike "USDC"/"usdc" entries can't coexist.
func TestGenesisValidate_DuplicateDisplayNameCaseInsensitive(t *testing.T) {
	gs := types.DefaultGenesis()
	clash := validBTC()
	clash.DisplayName = "uSdC"
	clash.MarginMode = perptypes.MarginModeDisabled
	gs.Assets = append(gs.Assets, clash)
	require.ErrorIs(t, gs.Validate(), types.ErrAssetExists)
}

func TestGenesisValidate_EmptyDenom(t *testing.T) {
	gs := types.DefaultGenesis()
	bad := validBTC()
	bad.Denom = ""
	gs.Assets = append(gs.Assets, bad)
	require.ErrorIs(t, gs.Validate(), types.ErrInvalidAssetParams)
}

func TestGenesisValidate_InvalidMarginMode(t *testing.T) {
	gs := types.DefaultGenesis()
	gs.Assets[0].MarginMode = 99
	require.ErrorIs(t, gs.Validate(), types.ErrInvalidAssetParams)
}

func TestGenesisValidate_NextIndexCollision(t *testing.T) {
	gs := types.DefaultGenesis()
	gs.NextAssetIndex = gs.Assets[0].AssetIndex
	require.ErrorIs(t, gs.Validate(), types.ErrInvalidModuleParams)
}

// NextAssetIndex must lie above every seeded asset_index.
func TestGenesisValidate_NextIndexBelowMaxSeeded(t *testing.T) {
	gs := types.DefaultGenesis()
	gs.NextAssetIndex = gs.Assets[0].AssetIndex - 1
	require.ErrorIs(t, gs.Validate(), types.ErrInvalidModuleParams)
}

// A 0 next_asset_index is accepted and treated as "uninitialised"
// (the keeper normalises it during InitGenesis).
func TestGenesisValidate_NextIndexZeroAllowed(t *testing.T) {
	gs := types.DefaultGenesis()
	gs.NextAssetIndex = 0
	require.NoError(t, gs.Validate())
}

// Decimals = 0 is rejected even though the proto allows it.
func TestGenesisValidate_DecimalsZero(t *testing.T) {
	gs := types.DefaultGenesis()
	gs.Assets[0].Decimals = 0
	require.ErrorIs(t, gs.Validate(), types.ErrInvalidAssetParams)
}

// Decimals > 18 is rejected.
func TestGenesisValidate_DecimalsTooLarge(t *testing.T) {
	gs := types.DefaultGenesis()
	gs.Assets[0].Decimals = 19
	require.ErrorIs(t, gs.Validate(), types.ErrInvalidAssetParams)
}

// extension_multiplier above the ceiling is rejected.
func TestGenesisValidate_ExtensionMultiplierTooLarge(t *testing.T) {
	gs := types.DefaultGenesis()
	gs.Assets[0].ExtensionMultiplier = perptypes.MaxExtensionMultiplier + 1
	require.ErrorIs(t, gs.Validate(), types.ErrInvalidAssetParams)
}

// display_name longer than the cap is rejected.
func TestGenesisValidate_DisplayNameTooLong(t *testing.T) {
	gs := types.DefaultGenesis()
	gs.Assets[0].DisplayName = strings.Repeat("A", perptypes.MaxAssetDisplayNameLen+1)
	require.ErrorIs(t, gs.Validate(), types.ErrInvalidAssetParams)
}

// A non-USDC asset with margin enabled trips the USDC binding.
func TestGenesisValidate_NonUSDCMarginEnabled(t *testing.T) {
	gs := types.DefaultGenesis()
	rogue := validBTC()
	rogue.MarginMode = perptypes.MarginModeEnabled
	gs.Assets = append(gs.Assets, rogue)
	require.ErrorIs(t, gs.Validate(), types.ErrUSDCMarginConstraint)
}

// An asset that *looks* like USDC (denom or display_name) but isn't
// at the canonical index trips the USDC binding. We construct a
// custom genesis without the default USDC seed so the dedicated USDC
// binding check (not the dup-name check) is the one that fires.
func TestGenesisValidate_LookAlikeUSDC(t *testing.T) {
	gs := &types.GenesisState{
		Params: types.DefaultParams(),
		Assets: []types.Asset{{
			AssetIndex:          4,
			Denom:               "uother",
			DisplayName:         "usdc",
			Decimals:            6,
			ExtensionMultiplier: 1,
			MinTransferAmount:   1,
			MinWithdrawalAmount: 1,
			MarginMode:          perptypes.MarginModeDisabled,
			Enabled:             true,
		}},
		NextAssetIndex: 5,
	}
	require.ErrorIs(t, gs.Validate(), types.ErrUSDCMarginConstraint)
}

// Same idea, but using the denom side of the binding.
func TestGenesisValidate_LookAlikeUSDCDenom(t *testing.T) {
	gs := &types.GenesisState{
		Params: types.DefaultParams(),
		Assets: []types.Asset{{
			AssetIndex:          4,
			Denom:               "uusdc",
			DisplayName:         "Other",
			Decimals:            6,
			ExtensionMultiplier: 1,
			MinTransferAmount:   1,
			MinWithdrawalAmount: 1,
			MarginMode:          perptypes.MarginModeDisabled,
			Enabled:             true,
		}},
		NextAssetIndex: 5,
	}
	require.ErrorIs(t, gs.Validate(), types.ErrUSDCMarginConstraint)
}

// The canonical USDC row must be margin-enabled.
func TestGenesisValidate_USDCMarginDisabledRejected(t *testing.T) {
	gs := types.DefaultGenesis()
	gs.Assets[0].MarginMode = perptypes.MarginModeDisabled
	require.ErrorIs(t, gs.Validate(), types.ErrUSDCMarginConstraint)
}

// asset_index outside [Min, Params.Max] is rejected.
func TestGenesisValidate_AssetIndexOutOfRange(t *testing.T) {
	gs := types.DefaultGenesis()
	bad := validBTC()
	bad.AssetIndex = perptypes.MaxAssetIndex + 1
	gs.Assets = append(gs.Assets, bad)
	require.ErrorIs(t, gs.Validate(), types.ErrInvalidAssetParams)
}
