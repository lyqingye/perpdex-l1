package types

import (
	"testing"

	sdk "github.com/cosmos/cosmos-sdk/types"

	"github.com/stretchr/testify/require"

	perptypes "github.com/perpdex/perpdex-l1/types"
)

const testAddr = "px1xqcnyve5x5mrwwpev93xxer9venks6t29ke4l8"

// configureBech32 sets the `px` prefix globally once per process. Tests in
// this package avoid importing `app` to dodge an import cycle.
func configureBech32() {
	cfg := sdk.GetConfig()
	cfg.SetBech32PrefixForAccount("px", "pxpub")
	cfg.SetBech32PrefixForValidator("pxvaloper", "pxvaloperpub")
	cfg.SetBech32PrefixForConsensusNode("pxvalcons", "pxvalconspub")
}

func TestMain(m *testing.M) {
	configureBech32()
	m.Run()
}

func TestCreateOrder_RejectsZeroBase(t *testing.T) {
	m := &MsgCreateOrder{
		Sender:      testAddr,
		BaseAmount:  0,
		OrderType:   perptypes.LimitOrder,
		TimeInForce: perptypes.GTT,
		Price:       100,
		Expiry:      1,
	}
	require.ErrorIs(t, m.ValidateBasic(), ErrInvalidOrder)
}

func TestCreateOrder_RejectsOversizedBase(t *testing.T) {
	m := &MsgCreateOrder{
		Sender:      testAddr,
		BaseAmount:  perptypes.MaxOrderBaseAmount + 1,
		OrderType:   perptypes.LimitOrder,
		TimeInForce: perptypes.GTT,
		Price:       100,
		Expiry:      1,
	}
	require.ErrorIs(t, m.ValidateBasic(), ErrInvalidOrder)
}

func TestCreateOrder_RejectsInvalidType(t *testing.T) {
	m := &MsgCreateOrder{
		Sender:      testAddr,
		BaseAmount:  1,
		OrderType:   12345,
		TimeInForce: perptypes.GTT,
	}
	require.ErrorIs(t, m.ValidateBasic(), ErrInvalidOrder)
}

func TestCreateOrder_RejectsInvalidTIF(t *testing.T) {
	m := &MsgCreateOrder{
		Sender:      testAddr,
		BaseAmount:  1,
		OrderType:   perptypes.LimitOrder,
		TimeInForce: 777,
		Price:       100,
	}
	require.ErrorIs(t, m.ValidateBasic(), ErrInvalidOrder)
}

func TestCreateOrder_LimitRequiresPrice(t *testing.T) {
	m := &MsgCreateOrder{
		Sender:      testAddr,
		BaseAmount:  1,
		OrderType:   perptypes.LimitOrder,
		TimeInForce: perptypes.GTT,
		Price:       0,
	}
	require.ErrorIs(t, m.ValidateBasic(), ErrInvalidOrder)
}

func TestCreateOrder_TriggerRequiresTriggerPrice(t *testing.T) {
	m := &MsgCreateOrder{
		Sender:       testAddr,
		BaseAmount:   1,
		OrderType:    perptypes.StopLossOrder,
		TimeInForce:  perptypes.IOC,
		TriggerPrice: 0,
	}
	require.ErrorIs(t, m.ValidateBasic(), ErrInvalidOrder)
}

func TestCreateOrder_PostOnlyLimitOnly(t *testing.T) {
	m := &MsgCreateOrder{
		Sender:      testAddr,
		BaseAmount:  1,
		OrderType:   perptypes.MarketOrder,
		TimeInForce: perptypes.PostOnly,
	}
	require.ErrorIs(t, m.ValidateBasic(), ErrInvalidOrder)
}

func TestCreateOrder_GTTRequiresExpiry(t *testing.T) {
	m := &MsgCreateOrder{
		Sender:      testAddr,
		BaseAmount:  1,
		OrderType:   perptypes.LimitOrder,
		TimeInForce: perptypes.GTT,
		Price:       100,
		Expiry:      0,
	}
	require.ErrorIs(t, m.ValidateBasic(), ErrInvalidOrder)
}

func TestCreateOrder_AcceptsHappyPath(t *testing.T) {
	m := &MsgCreateOrder{
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
	m := &MsgCancelAllOrders{Sender: testAddr, Mode: 99}
	require.ErrorIs(t, m.ValidateBasic(), ErrInvalidOrder)
}

func TestModifyOrder_RejectsZero(t *testing.T) {
	m := &MsgModifyOrder{Sender: testAddr, NewBaseAmount: 0}
	require.ErrorIs(t, m.ValidateBasic(), ErrInvalidOrder)
}
