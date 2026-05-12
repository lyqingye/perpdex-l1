package types

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	perptypes "github.com/perpdex/perpdex-l1/types"
)

// validBTC returns a well-formed non-margin asset suitable for
// concatenating onto DefaultGenesis().Assets without tripping the
// per-row validator. Tests override individual fields to exercise
// specific failure modes.
func validBTC() Asset {
	return Asset{
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

func TestGenesisValidate_OK(t *testing.T) {
	gs := DefaultGenesis()
	require.NoError(t, gs.Validate())
}

func TestGenesisValidate_DuplicateIndex(t *testing.T) {
	gs := DefaultGenesis()
	clash := validBTC()
	clash.AssetIndex = gs.Assets[0].AssetIndex
	clash.Denom = "uother"
	clash.DisplayName = "OTHER"
	gs.Assets = append(gs.Assets, clash)
	require.ErrorIs(t, gs.Validate(), ErrAssetExists)
}

func TestGenesisValidate_DuplicateDenom(t *testing.T) {
	gs := DefaultGenesis()
	clash := validBTC()
	clash.Denom = gs.Assets[0].Denom
	clash.DisplayName = "OTHER"
	gs.Assets = append(gs.Assets, clash)
	require.ErrorIs(t, gs.Validate(), ErrAssetExists)
}

// New: case-insensitive duplicate display_name must be rejected so
// look-alike "USDC"/"usdc" entries can't coexist.
func TestGenesisValidate_DuplicateDisplayNameCaseInsensitive(t *testing.T) {
	gs := DefaultGenesis()
	clash := validBTC()
	clash.DisplayName = "uSdC"
	clash.MarginMode = perptypes.MarginModeDisabled
	gs.Assets = append(gs.Assets, clash)
	require.ErrorIs(t, gs.Validate(), ErrAssetExists)
}

func TestGenesisValidate_EmptyDenom(t *testing.T) {
	gs := DefaultGenesis()
	bad := validBTC()
	bad.Denom = ""
	gs.Assets = append(gs.Assets, bad)
	require.ErrorIs(t, gs.Validate(), ErrInvalidAssetParams)
}

func TestGenesisValidate_InvalidMarginMode(t *testing.T) {
	gs := DefaultGenesis()
	gs.Assets[0].MarginMode = 99
	require.ErrorIs(t, gs.Validate(), ErrInvalidAssetParams)
}

func TestGenesisValidate_NextIndexCollision(t *testing.T) {
	gs := DefaultGenesis()
	gs.NextAssetIndex = gs.Assets[0].AssetIndex
	require.ErrorIs(t, gs.Validate(), ErrInvalidModuleParams)
}

// New: NextAssetIndex must lie above every seeded asset_index.
func TestGenesisValidate_NextIndexBelowMaxSeeded(t *testing.T) {
	gs := DefaultGenesis()
	gs.NextAssetIndex = gs.Assets[0].AssetIndex - 1
	require.ErrorIs(t, gs.Validate(), ErrInvalidModuleParams)
}

// New: a 0 next_asset_index is accepted and treated as "uninitialised"
// (the keeper normalises it during InitGenesis).
func TestGenesisValidate_NextIndexZeroAllowed(t *testing.T) {
	gs := DefaultGenesis()
	gs.NextAssetIndex = 0
	require.NoError(t, gs.Validate())
}

// New: decimals = 0 should be rejected even though the proto allows it.
func TestGenesisValidate_DecimalsZero(t *testing.T) {
	gs := DefaultGenesis()
	gs.Assets[0].Decimals = 0
	require.ErrorIs(t, gs.Validate(), ErrInvalidAssetParams)
}

// New: decimals > 18 is rejected.
func TestGenesisValidate_DecimalsTooLarge(t *testing.T) {
	gs := DefaultGenesis()
	gs.Assets[0].Decimals = 19
	require.ErrorIs(t, gs.Validate(), ErrInvalidAssetParams)
}

// New: extension_multiplier above the ceiling is rejected.
func TestGenesisValidate_ExtensionMultiplierTooLarge(t *testing.T) {
	gs := DefaultGenesis()
	gs.Assets[0].ExtensionMultiplier = MaxExtensionMultiplier + 1
	require.ErrorIs(t, gs.Validate(), ErrInvalidAssetParams)
}

// New: display_name longer than the cap is rejected.
func TestGenesisValidate_DisplayNameTooLong(t *testing.T) {
	gs := DefaultGenesis()
	gs.Assets[0].DisplayName = strings.Repeat("A", MaxAssetDisplayNameLen+1)
	require.ErrorIs(t, gs.Validate(), ErrInvalidAssetParams)
}

// New: a non-USDC asset with margin enabled trips the USDC binding.
func TestGenesisValidate_NonUSDCMarginEnabled(t *testing.T) {
	gs := DefaultGenesis()
	rogue := validBTC()
	rogue.MarginMode = perptypes.MarginModeEnabled
	gs.Assets = append(gs.Assets, rogue)
	require.ErrorIs(t, gs.Validate(), ErrUSDCMarginConstraint)
}

// New: an asset that *looks* like USDC (denom or display_name) but
// isn't at the canonical index trips the USDC binding. We construct a
// custom genesis without the default USDC seed so the dedicated USDC
// binding check (not the dup-name check) is the one that fires.
func TestGenesisValidate_LookAlikeUSDC(t *testing.T) {
	gs := &GenesisState{
		Params: DefaultParams(),
		Assets: []Asset{{
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
	require.ErrorIs(t, gs.Validate(), ErrUSDCMarginConstraint)
}

// New: same idea, but using the denom side of the binding.
func TestGenesisValidate_LookAlikeUSDCDenom(t *testing.T) {
	gs := &GenesisState{
		Params: DefaultParams(),
		Assets: []Asset{{
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
	require.ErrorIs(t, gs.Validate(), ErrUSDCMarginConstraint)
}

// New: the canonical USDC row must be margin-enabled.
func TestGenesisValidate_USDCMarginDisabledRejected(t *testing.T) {
	gs := DefaultGenesis()
	gs.Assets[0].MarginMode = perptypes.MarginModeDisabled
	require.ErrorIs(t, gs.Validate(), ErrUSDCMarginConstraint)
}

// New: asset_index outside [Min, Params.Max] is rejected.
func TestGenesisValidate_AssetIndexOutOfRange(t *testing.T) {
	gs := DefaultGenesis()
	bad := validBTC()
	bad.AssetIndex = perptypes.MaxAssetIndex + 1
	gs.Assets = append(gs.Assets, bad)
	require.ErrorIs(t, gs.Validate(), ErrInvalidAssetParams)
}
