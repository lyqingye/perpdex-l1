// msg_register_test.go covers MsgServer.RegisterAsset: the governance
// path that mints new asset rows at runtime. The tests pin down the
// happy path (correct index + event emission) plus every rejection
// branch — authority gating, duplicate-denom / duplicate-display-name
// collisions, the USDC reserved-name binding, the shape validators on
// decimals / extension multiplier / denom / display_name, and the
// MaxAssetIndex cap.
package tests

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	perptypes "github.com/perpdex/perpdex-l1/types"
	"github.com/perpdex/perpdex-l1/x/asset/types"
)

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
