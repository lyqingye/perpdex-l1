// Pure types-level Msg ValidateBasic coverage. These tests pin the
// stateless invariants that fire before any handler logic: route /
// trading-mode / margin-mode / margin-action enums, IMF ceiling,
// public pool bounds, and the strategy-bucket index range. Keeping
// them isolated from the keeper means a regression in the stateless
// guard is caught even if the matching msg_server defense-in-depth
// check is accidentally weakened.
package tests

import (
	"testing"

	"github.com/stretchr/testify/require"

	"cosmossdk.io/math"

	perptypes "github.com/perpdex/perpdex-l1/types"
	"github.com/perpdex/perpdex-l1/x/account/types"
)

// TestMsgDeposit_ValidateBasic_RouteEnum locks in that the
// route_type ∈ {Perps, Spot} guard is enforced at the ValidateBasic
// layer (not at the msg_server handler layer).
func TestMsgDeposit_ValidateBasic_RouteEnum(t *testing.T) {
	base := types.MsgDeposit{
		Sender:     validOwner,
		AssetIndex: perptypes.USDCAssetIndex,
		Amount:     1_000_000,
	}

	cases := []struct {
		name  string
		route uint32
		ok    bool
	}{
		{"perps_ok", perptypes.RouteTypePerps, true},
		{"spot_ok", perptypes.RouteTypeSpot, true},
		{"unknown_route_rejected", 99, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			m := base
			m.RouteType = tc.route
			err := m.ValidateBasic()
			if tc.ok {
				require.NoError(t, err)
				return
			}
			require.ErrorIs(t, err, types.ErrInvalidRoute)
		})
	}
}

func TestMsgWithdraw_ValidateBasic_RouteEnum(t *testing.T) {
	base := types.MsgWithdraw{
		Sender:       validOwner,
		AccountIndex: 100,
		AssetIndex:   perptypes.USDCAssetIndex,
		Amount:       1_000_000,
	}
	m := base
	m.RouteType = 42
	require.ErrorIs(t, m.ValidateBasic(), types.ErrInvalidRoute)
}

func TestMsgUpdateAccountConfig_ValidateBasic_TradingModeEnum(t *testing.T) {
	m := types.MsgUpdateAccountConfig{
		Sender:         validOwner,
		AccountIndex:   100,
		NewTradingMode: 99,
	}
	require.ErrorIs(t, m.ValidateBasic(), types.ErrInvalidTradingMode)
}

func TestMsgUpdateAccountAssetConfig_ValidateBasic_MarginModeEnum(t *testing.T) {
	m := types.MsgUpdateAccountAssetConfig{
		Sender:        validOwner,
		AccountIndex:  100,
		AssetIndex:    perptypes.USDCAssetIndex,
		NewMarginMode: 99,
	}
	require.ErrorIs(t, m.ValidateBasic(), types.ErrInvalidMarginMode)
}

func TestMsgUpdateMargin_ValidateBasic_ActionEnum(t *testing.T) {
	m := types.MsgUpdateMargin{
		Sender:       validOwner,
		AccountIndex: 100,
		MarketIndex:  0,
		Action:       99,
		Amount:       math.NewInt(1),
	}
	require.ErrorIs(t, m.ValidateBasic(), types.ErrInvalidMarginAction)
}

func TestMsgUpdateLeverage_ValidateBasic_MarginModeAndIMFCeiling(t *testing.T) {
	base := types.MsgUpdateLeverage{
		Sender:                   validOwner,
		AccountIndex:             100,
		MarketIndex:              0,
		NewInitialMarginFraction: 1000,
		NewMarginMode:            perptypes.CrossMargin,
	}

	t.Run("rejects_unknown_margin_mode", func(t *testing.T) {
		m := base
		m.NewMarginMode = 99
		require.ErrorIs(t, m.ValidateBasic(), types.ErrInvalidMarginMode)
	})

	t.Run("rejects_imf_above_margin_tick", func(t *testing.T) {
		m := base
		m.NewInitialMarginFraction = perptypes.MarginTick + 1
		require.ErrorIs(t, m.ValidateBasic(), types.ErrInvalidParams)
	})

	t.Run("accepts_valid", func(t *testing.T) {
		require.NoError(t, base.ValidateBasic())
	})
}

func TestMsgCreatePublicPool_ValidateBasic_Bounds(t *testing.T) {
	good := types.MsgCreatePublicPool{
		Sender:               validOwner,
		MasterAccountIndex:   100,
		AccountType:          perptypes.PublicPoolAccountType,
		InitialTotalShares:   10,
		OperatorFee:          1000,
		MinOperatorShareRate: perptypes.ShareTick,
	}
	require.NoError(t, good.ValidateBasic())

	t.Run("rejects_non_public_pool_type", func(t *testing.T) {
		m := good
		m.AccountType = perptypes.InsuranceFundAccountType
		require.ErrorIs(t, m.ValidateBasic(), types.ErrInvalidAccountType)
	})
	t.Run("rejects_operator_fee_ge_fee_tick", func(t *testing.T) {
		m := good
		m.OperatorFee = uint32(perptypes.FeeTick)
		require.ErrorIs(t, m.ValidateBasic(), types.ErrInvalidParams)
	})
	t.Run("rejects_min_rate_above_share_tick", func(t *testing.T) {
		m := good
		m.MinOperatorShareRate = perptypes.ShareTick + 1
		require.ErrorIs(t, m.ValidateBasic(), types.ErrInvalidParams)
	})
}

func TestMsgUpdatePublicPool_ValidateBasic_StatusEnum(t *testing.T) {
	good := types.MsgUpdatePublicPool{
		Sender:                  validOwner,
		PoolAccountIndex:        100,
		NewStatus:               perptypes.PublicPoolStatusActive,
		NewMinOperatorShareRate: perptypes.ShareTick,
	}
	require.NoError(t, good.ValidateBasic())

	t.Run("rejects_unknown_status", func(t *testing.T) {
		m := good
		m.NewStatus = 99
		require.ErrorIs(t, m.ValidateBasic(), types.ErrInvalidPoolUpdate)
	})
	t.Run("rejects_min_rate_above_share_tick", func(t *testing.T) {
		m := good
		m.NewMinOperatorShareRate = perptypes.ShareTick + 1
		require.ErrorIs(t, m.ValidateBasic(), types.ErrInvalidParams)
	})
}

func TestMsgStrategyTransfer_ValidateBasic_BucketBounds(t *testing.T) {
	good := types.MsgStrategyTransfer{
		Sender:           validOwner,
		PoolAccountIndex: 100,
		FromStrategy:     0,
		ToStrategy:       1,
		Amount:           math.NewInt(100),
	}
	require.NoError(t, good.ValidateBasic())

	t.Run("rejects_from_out_of_range", func(t *testing.T) {
		m := good
		m.FromStrategy = uint32(perptypes.NbStrategies)
		require.ErrorIs(t, m.ValidateBasic(), types.ErrInvalidStrategyIdx)
	})
	t.Run("rejects_to_out_of_range", func(t *testing.T) {
		m := good
		m.ToStrategy = uint32(perptypes.NbStrategies)
		require.ErrorIs(t, m.ValidateBasic(), types.ErrInvalidStrategyIdx)
	})
}
