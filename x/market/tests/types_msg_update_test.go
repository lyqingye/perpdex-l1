// types_msg_update_test.go pins ValidateBasic for the governance
// update path: MsgUpdateMarket, MsgUpdateMarketDetails and
// MsgUpdateParams. Includes the Params.IsValidIndex helper-level
// assertions that lock the live perp / spot index range invariants.
package tests

import (
	"testing"

	"github.com/stretchr/testify/require"

	perptypes "github.com/perpdex/perpdex-l1/types"
	"github.com/perpdex/perpdex-l1/x/market/types"
)

func TestMsgUpdateMarket_Valid(t *testing.T) {
	require.NoError(t, validUpdateMarketMsg().ValidateBasic())
}

func TestMsgUpdateMarket_BadAuthority(t *testing.T) {
	m := validUpdateMarketMsg()
	m.Authority = ""
	require.Error(t, m.ValidateBasic())
}

func TestMsgUpdateMarket_NilMarketIndex(t *testing.T) {
	m := validUpdateMarketMsg()
	m.MarketIndex = perptypes.NilMarketIndex
	require.ErrorIs(t, m.ValidateBasic(), types.ErrMarketIndexExceed)
}

func TestMsgUpdateMarket_BadStatus(t *testing.T) {
	m := validUpdateMarketMsg()
	m.NewStatus = 99
	require.ErrorIs(t, m.ValidateBasic(), types.ErrInvalidMarket)
}

func TestMsgUpdateMarket_FeeTooHigh(t *testing.T) {
	cases := []struct {
		name string
		mut  func(*types.MsgUpdateMarket)
	}{
		{"taker", func(m *types.MsgUpdateMarket) { m.NewTakerFee = uint32(perptypes.FeeTick) }},
		{"maker", func(m *types.MsgUpdateMarket) { m.NewMakerFee = uint32(perptypes.FeeTick) }},
		{"liquidation", func(m *types.MsgUpdateMarket) { m.NewLiquidationFee = uint32(perptypes.FeeTick) }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			m := validUpdateMarketMsg()
			tc.mut(m)
			require.ErrorIs(t, m.ValidateBasic(), types.ErrInvalidParams)
		})
	}
}

func TestMsgUpdateMarket_ZeroMinAmounts(t *testing.T) {
	cases := []string{"base", "quote"}
	for _, name := range cases {
		t.Run(name, func(t *testing.T) {
			m := validUpdateMarketMsg()
			if name == "base" {
				m.NewMinBaseAmount = 0
			} else {
				m.NewMinQuoteAmount = 0
			}
			require.ErrorIs(t, m.ValidateBasic(), types.ErrInvalidParams)
		})
	}
}

func TestMsgUpdateMarket_NegativeExpiry(t *testing.T) {
	m := validUpdateMarketMsg()
	m.NewExpiryTimestamp = -1
	require.ErrorIs(t, m.ValidateBasic(), types.ErrInvalidMarket)
}

func TestMsgUpdateMarket_ExpiredWithFutureExpiry(t *testing.T) {
	m := validUpdateMarketMsg()
	m.NewStatus = perptypes.MarketStatusExpired
	m.NewExpiryTimestamp = 1_700_000_000_000
	require.ErrorIs(t, m.ValidateBasic(), types.ErrInvalidMarket)
}

func TestMsgUpdateMarket_NegativeOrderQuoteLimit(t *testing.T) {
	m := validUpdateMarketMsg()
	m.NewOrderQuoteLimit = -1
	require.ErrorIs(t, m.ValidateBasic(), types.ErrInvalidParams)
}

func TestMsgUpdateMarketDetails_Valid(t *testing.T) {
	require.NoError(t, validUpdateMarketDetailsMsg().ValidateBasic())
}

func TestMsgUpdateMarketDetails_BadAuthority(t *testing.T) {
	m := validUpdateMarketDetailsMsg()
	m.Authority = ""
	require.Error(t, m.ValidateBasic())
}

func TestMsgUpdateMarketDetails_NilMarketIndex(t *testing.T) {
	m := validUpdateMarketDetailsMsg()
	m.MarketIndex = perptypes.NilMarketIndex
	require.ErrorIs(t, m.ValidateBasic(), types.ErrMarketIndexExceed)
}

func TestMsgUpdateMarketDetails_MinImfZero(t *testing.T) {
	m := validUpdateMarketDetailsMsg()
	m.NewMinImf = 0
	require.ErrorIs(t, m.ValidateBasic(), types.ErrInvalidParams)
}

func TestMsgUpdateMarketDetails_IMFOverMarginTick(t *testing.T) {
	m := validUpdateMarketDetailsMsg()
	m.NewDefaultImf = uint32(perptypes.MarginTick) + 1
	require.ErrorIs(t, m.ValidateBasic(), types.ErrInvalidParams)
}

func TestMsgUpdateMarketDetails_MarginChainViolated(t *testing.T) {
	m := validUpdateMarketDetailsMsg()
	m.NewMaintenanceMf = m.NewDefaultImf // violates strict <
	require.ErrorIs(t, m.ValidateBasic(), types.ErrInvalidMarginChain)
}

func TestMsgUpdateMarketDetails_FundingClampReversed(t *testing.T) {
	m := validUpdateMarketDetailsMsg()
	m.NewFundingClampSmall = m.NewFundingClampBig + 1
	require.ErrorIs(t, m.ValidateBasic(), types.ErrInvalidParams)
}

func TestMsgUpdateMarketDetails_InterestRateTooHigh(t *testing.T) {
	m := validUpdateMarketDetailsMsg()
	m.NewInterestRate = uint32(perptypes.FundingRateTick + 1)
	require.ErrorIs(t, m.ValidateBasic(), types.ErrInvalidParams)
}

func TestMsgUpdateMarketDetails_OpenInterestLimitTooHigh(t *testing.T) {
	m := validUpdateMarketDetailsMsg()
	m.NewOpenInterestLimit = perptypes.MaxOrderBaseAmount + 1
	require.ErrorIs(t, m.ValidateBasic(), types.ErrInvalidParams)
}

func TestMsgUpdateParams_Valid(t *testing.T) {
	m := &types.MsgUpdateParams{
		Authority: testAuthority,
		Params:    types.DefaultParams(),
	}
	require.NoError(t, m.ValidateBasic())
}

func TestMsgUpdateParams_BadAuthority(t *testing.T) {
	m := &types.MsgUpdateParams{
		Authority: "",
		Params:    types.DefaultParams(),
	}
	require.Error(t, m.ValidateBasic())
}

func TestMsgUpdateParams_PerpsOverlapSpot(t *testing.T) {
	m := &types.MsgUpdateParams{
		Authority: testAuthority,
		Params: types.Params{
			MaxPerpsMarketIndex:       perptypes.MinSpotMarketIndex,
			MinSpotMarketIndex:        perptypes.MinSpotMarketIndex,
			MaxSpotMarketIndex:        perptypes.MaxSpotMarketIndex,
			MaxMarketsExpiredPerBlock: 32,
		},
	}
	require.ErrorIs(t, m.ValidateBasic(), types.ErrInvalidParams)
}

func TestMsgUpdateParams_PerpsAboveNilMarketIndex(t *testing.T) {
	m := &types.MsgUpdateParams{
		Authority: testAuthority,
		Params: types.Params{
			MaxPerpsMarketIndex:       perptypes.NilMarketIndex,
			MinSpotMarketIndex:        perptypes.MinSpotMarketIndex,
			MaxSpotMarketIndex:        perptypes.MaxSpotMarketIndex,
			MaxMarketsExpiredPerBlock: 32,
		},
	}
	require.ErrorIs(t, m.ValidateBasic(), types.ErrInvalidParams)
}

func TestMsgUpdateParams_ZeroSpotIndex(t *testing.T) {
	m := &types.MsgUpdateParams{
		Authority: testAuthority,
		Params: types.Params{
			MaxPerpsMarketIndex:       0,
			MinSpotMarketIndex:        0,
			MaxSpotMarketIndex:        0,
			MaxMarketsExpiredPerBlock: 32,
		},
	}
	require.ErrorIs(t, m.ValidateBasic(), types.ErrInvalidParams)
}

func TestMsgUpdateParams_MaxMarketsZero(t *testing.T) {
	// 0 is a valid emergency switch — auto-expiry disabled.
	m := &types.MsgUpdateParams{
		Authority: testAuthority,
		Params: types.Params{
			MaxPerpsMarketIndex:       perptypes.MaxPerpsMarketIndex,
			MinSpotMarketIndex:        perptypes.MinSpotMarketIndex,
			MaxSpotMarketIndex:        perptypes.MaxSpotMarketIndex,
			MaxMarketsExpiredPerBlock: 0,
		},
	}
	require.NoError(t, m.ValidateBasic())
}

func TestParams_IsValidIndex(t *testing.T) {
	p := types.DefaultParams()
	require.True(t, p.IsValidIndex(1))
	require.True(t, p.IsValidIndex(perptypes.MaxPerpsMarketIndex))
	require.True(t, p.IsValidIndex(perptypes.MinSpotMarketIndex))
	require.True(t, p.IsValidIndex(perptypes.MaxSpotMarketIndex))
	require.False(t, p.IsValidIndex(perptypes.NilMarketIndex))
	require.False(t, p.IsValidIndex(perptypes.MaxPerpsMarketIndex+1))
}
