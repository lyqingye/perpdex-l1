// msg_update_test.go covers the mutating MsgServer endpoints other
// than registration:
//
//   - UpdateAsset:  per-asset adjustments to min amounts / enabled flag,
//     including the USDC invariant that the canonical row must stay
//     enabled and the authority gate.
//   - UpdateParams: module-level parameter changes (MaxAssetIndex),
//     including the zero-cap rejection and the authority gate.
package tests

import (
	"testing"

	"github.com/stretchr/testify/require"

	perptypes "github.com/perpdex/perpdex-l1/types"
	"github.com/perpdex/perpdex-l1/x/asset/types"
)

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
