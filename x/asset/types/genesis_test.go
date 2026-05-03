package types

import (
	"testing"

	"github.com/stretchr/testify/require"

	perptypes "github.com/perpdex/perpdex-l1/types"
)

// TestGenesisValidate_OK asserts the default genesis passes validation.
func TestGenesisValidate_OK(t *testing.T) {
	gs := DefaultGenesis()
	require.NoError(t, gs.Validate())
}

// TestGenesisValidate_DuplicateIndex rejects duplicate asset_index.
func TestGenesisValidate_DuplicateIndex(t *testing.T) {
	gs := DefaultGenesis()
	gs.Assets = append(gs.Assets, Asset{
		AssetIndex:          gs.Assets[0].AssetIndex, // duplicate
		Denom:               "uusdt",
		ExtensionMultiplier: 1,
		MarginMode:          perptypes.MarginModeDisabled,
	})
	require.ErrorIs(t, gs.Validate(), ErrAssetExists)
}

// TestGenesisValidate_DuplicateDenom rejects duplicate denom strings.
func TestGenesisValidate_DuplicateDenom(t *testing.T) {
	gs := DefaultGenesis()
	gs.Assets = append(gs.Assets, Asset{
		AssetIndex:          gs.Assets[0].AssetIndex + 1,
		Denom:               gs.Assets[0].Denom, // duplicate denom
		ExtensionMultiplier: 1,
		MarginMode:          perptypes.MarginModeDisabled,
	})
	require.ErrorIs(t, gs.Validate(), ErrAssetExists)
}

// TestGenesisValidate_EmptyDenom rejects an asset with no denom.
func TestGenesisValidate_EmptyDenom(t *testing.T) {
	gs := DefaultGenesis()
	gs.Assets = append(gs.Assets, Asset{
		AssetIndex:          gs.Assets[0].AssetIndex + 1,
		Denom:               "",
		ExtensionMultiplier: 1,
		MarginMode:          perptypes.MarginModeDisabled,
	})
	require.ErrorIs(t, gs.Validate(), ErrInvalidParams)
}

// TestGenesisValidate_InvalidMarginMode rejects out-of-range margin enum.
func TestGenesisValidate_InvalidMarginMode(t *testing.T) {
	gs := DefaultGenesis()
	gs.Assets[0].MarginMode = 99
	require.ErrorIs(t, gs.Validate(), ErrInvalidParams)
}

// TestGenesisValidate_NextIndexCollision rejects a next_asset_index that
// points at an already-seeded asset.
func TestGenesisValidate_NextIndexCollision(t *testing.T) {
	gs := DefaultGenesis()
	gs.NextAssetIndex = gs.Assets[0].AssetIndex
	require.ErrorIs(t, gs.Validate(), ErrInvalidParams)
}
