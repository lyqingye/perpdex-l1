// types_msgs_test.go covers MsgCreateOrder / MsgCancelAllOrders /
// MsgModifyOrder ValidateBasic, which is the only invariant gate in
// msg_server before any orderbook mutation. The matrix mirrors the
// previous `x/matching/types/msgs_test.go` content; it lives here
// (as the external `tests` package) so the same TestMain bech32
// wiring serves both the keeper-level and types-level tests.
package tests

import (
	"testing"

	"github.com/stretchr/testify/require"

	perptypes "github.com/perpdex/perpdex-l1/types"
	matchingtypes "github.com/perpdex/perpdex-l1/x/matching/types"
)

const testAddr = "px1xqcnyve5x5mrwwpev93xxer9venks6t29ke4l8"

func TestCreateOrder_RejectsZeroBase(t *testing.T) {
	m := &matchingtypes.MsgCreateOrder{
		Sender:      testAddr,
		BaseAmount:  0,
		OrderType:   perptypes.LimitOrder,
		TimeInForce: perptypes.GTT,
		Price:       100,
		Expiry:      1,
	}
	require.ErrorIs(t, m.ValidateBasic(), matchingtypes.ErrInvalidOrder)
}

func TestCreateOrder_RejectsOversizedBase(t *testing.T) {
	m := &matchingtypes.MsgCreateOrder{
		Sender:      testAddr,
		BaseAmount:  perptypes.MaxOrderBaseAmount + 1,
		OrderType:   perptypes.LimitOrder,
		TimeInForce: perptypes.GTT,
		Price:       100,
		Expiry:      1,
	}
	require.ErrorIs(t, m.ValidateBasic(), matchingtypes.ErrInvalidOrder)
}

func TestCreateOrder_RejectsInvalidType(t *testing.T) {
	m := &matchingtypes.MsgCreateOrder{
		Sender:      testAddr,
		BaseAmount:  1,
		OrderType:   12345,
		TimeInForce: perptypes.GTT,
	}
	require.ErrorIs(t, m.ValidateBasic(), matchingtypes.ErrInvalidOrder)
}

func TestCreateOrder_RejectsInvalidTIF(t *testing.T) {
	m := &matchingtypes.MsgCreateOrder{
		Sender:      testAddr,
		BaseAmount:  1,
		OrderType:   perptypes.LimitOrder,
		TimeInForce: 777,
		Price:       100,
	}
	require.ErrorIs(t, m.ValidateBasic(), matchingtypes.ErrInvalidOrder)
}

func TestCreateOrder_LimitRequiresPrice(t *testing.T) {
	m := &matchingtypes.MsgCreateOrder{
		Sender:      testAddr,
		BaseAmount:  1,
		OrderType:   perptypes.LimitOrder,
		TimeInForce: perptypes.GTT,
		Price:       0,
	}
	require.ErrorIs(t, m.ValidateBasic(), matchingtypes.ErrInvalidOrder)
}

func TestCreateOrder_TriggerRequiresTriggerPrice(t *testing.T) {
	m := &matchingtypes.MsgCreateOrder{
		Sender:       testAddr,
		BaseAmount:   1,
		OrderType:    perptypes.StopLossOrder,
		TimeInForce:  perptypes.IOC,
		TriggerPrice: 0,
	}
	require.ErrorIs(t, m.ValidateBasic(), matchingtypes.ErrInvalidOrder)
}

func TestCreateOrder_PostOnlyLimitOnly(t *testing.T) {
	m := &matchingtypes.MsgCreateOrder{
		Sender:      testAddr,
		BaseAmount:  1,
		OrderType:   perptypes.MarketOrder,
		TimeInForce: perptypes.PostOnly,
	}
	require.ErrorIs(t, m.ValidateBasic(), matchingtypes.ErrInvalidOrder)
}

func TestCreateOrder_GTTRequiresExpiry(t *testing.T) {
	m := &matchingtypes.MsgCreateOrder{
		Sender:      testAddr,
		BaseAmount:  1,
		OrderType:   perptypes.LimitOrder,
		TimeInForce: perptypes.GTT,
		Price:       100,
		Expiry:      0,
	}
	require.ErrorIs(t, m.ValidateBasic(), matchingtypes.ErrInvalidOrder)
}

func TestCreateOrder_AcceptsHappyPath(t *testing.T) {
	m := &matchingtypes.MsgCreateOrder{
		Sender:      testAddr,
		BaseAmount:  1,
		OrderType:   perptypes.LimitOrder,
		TimeInForce: perptypes.GTT,
		Price:       100,
		Expiry:      1,
	}
	require.NoError(t, m.ValidateBasic())
}

func TestCancelAllOrders_RejectsBadMode(t *testing.T) {
	m := &matchingtypes.MsgCancelAllOrders{Sender: testAddr, Mode: 99}
	require.ErrorIs(t, m.ValidateBasic(), matchingtypes.ErrInvalidOrder)
}

func TestModifyOrder_RejectsZero(t *testing.T) {
	m := &matchingtypes.MsgModifyOrder{Sender: testAddr, NewBaseAmount: 0}
	require.ErrorIs(t, m.ValidateBasic(), matchingtypes.ErrInvalidOrder)
}
