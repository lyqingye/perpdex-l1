// Keeper-level genesis round-trip coverage. Distinct from the pure
// types-level genesis validation tests in types_genesis_test.go: this
// suite exercises the InitGenesis / ExportGenesis pair on a live
// keeper to make sure account-assets, positions and metas survive a
// full export + import cycle without losing rows.
package tests

import (
	"testing"

	"github.com/stretchr/testify/require"

	"cosmossdk.io/collections"
	"cosmossdk.io/math"

	perptypes "github.com/perpdex/perpdex-l1/types"
	"github.com/perpdex/perpdex-l1/x/account/types"
)

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
