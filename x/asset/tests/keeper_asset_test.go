// keeper_asset_test.go exercises the keeper-level asset CRUD helpers
// (CreateAsset / UpdateAsset) directly, bypassing the MsgServer.
// These are storage-layer invariants:
//
//   - CreateAsset rejects the nil index, duplicate index, and duplicate
//     denom, and persists both the primary record and the denom index
//     on success.
//   - UpdateAsset rejects the nil index and missing rows, and — most
//     importantly — treats denom as immutable so the asset's identity
//     can never drift after creation.
package tests

import (
	"testing"

	"github.com/stretchr/testify/require"

	perptypes "github.com/perpdex/perpdex-l1/types"
	"github.com/perpdex/perpdex-l1/x/asset/types"
)

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
