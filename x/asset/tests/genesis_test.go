// genesis_test.go covers the keeper's genesis lifecycle:
//
//   - InitGenesis seeds the canonical USDC row at the reserved index
//     and bumps the NextAssetIndex sequence past it.
//   - A genesis carrying NextAssetIndex == 0 is treated as
//     "uninitialised" and the keeper normalises it above MinAssetIndex.
//   - ExportGenesis → InitGenesis round-trips arbitrary state without
//     losing seeded rows, registered rows, or the sequence pointer.
package tests

import (
	"testing"

	"github.com/stretchr/testify/require"

	perptypes "github.com/perpdex/perpdex-l1/types"
	"github.com/perpdex/perpdex-l1/x/asset/types"
)

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
