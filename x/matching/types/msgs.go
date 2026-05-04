package types

import (
	sdk "github.com/cosmos/cosmos-sdk/types"
	sdkerrors "github.com/cosmos/cosmos-sdk/types/errors"

	perptypes "github.com/perpdex/perpdex-l1/types"
)

var (
	_ sdk.Msg = (*MsgCreateOrder)(nil)
	_ sdk.Msg = (*MsgCancelOrder)(nil)
	_ sdk.Msg = (*MsgCancelAllOrders)(nil)
	_ sdk.Msg = (*MsgModifyOrder)(nil)
	_ sdk.Msg = (*MsgUpdateParams)(nil)
)

func validAddr(s string) error {
	if _, err := sdk.AccAddressFromBech32(s); err != nil {
		return sdkerrors.ErrInvalidAddress.Wrap(err.Error())
	}
	return nil
}

// validateOrderType accepts the full on-chain order_type enum (see
// types/constants.go).
func validateOrderType(t uint32) bool {
	switch t {
	case perptypes.LimitOrder,
		perptypes.MarketOrder,
		perptypes.StopLossOrder,
		perptypes.StopLossLimitOrder,
		perptypes.TakeProfitOrder,
		perptypes.TakeProfitLimitOrder,
		perptypes.TWAPOrder,
		perptypes.TWAPSubOrder,
		perptypes.LiquidationOrder:
		return true
	}
	return false
}

func isTriggerOrderType(t uint32) bool {
	return t == perptypes.StopLossOrder ||
		t == perptypes.StopLossLimitOrder ||
		t == perptypes.TakeProfitOrder ||
		t == perptypes.TakeProfitLimitOrder
}

func requiresLimitPrice(t uint32) bool {
	return t == perptypes.LimitOrder ||
		t == perptypes.StopLossLimitOrder ||
		t == perptypes.TakeProfitLimitOrder ||
		t == perptypes.TWAPSubOrder ||
		t == perptypes.LiquidationOrder
}

func validateTIF(t uint32) bool {
	switch t {
	case perptypes.IOC, perptypes.GTT, perptypes.PostOnly:
		return true
	}
	return false
}

func (m *MsgCreateOrder) ValidateBasic() error {
	if err := validAddr(m.Sender); err != nil {
		return err
	}
	if m.BaseAmount == 0 || m.BaseAmount > perptypes.MaxOrderBaseAmount {
		return ErrInvalidOrder.Wrapf("base_amount out of range (got %d)", m.BaseAmount)
	}
	if !validateOrderType(m.OrderType) {
		return ErrInvalidOrder.Wrapf("order_type=%d", m.OrderType)
	}
	if !validateTIF(m.TimeInForce) {
		return ErrInvalidOrder.Wrapf("time_in_force=%d", m.TimeInForce)
	}
	if m.ClientOrderIndex != 0 {
		if m.ClientOrderIndex < perptypes.MinClientOrderIndex ||
			m.ClientOrderIndex > perptypes.MaxClientOrderIndex {
			return ErrInvalidOrder.Wrapf("client_order_index=%d out of range", m.ClientOrderIndex)
		}
	}
	if requiresLimitPrice(m.OrderType) && m.Price == 0 {
		return ErrInvalidOrder.Wrap("limit price must be > 0 for limit-style orders")
	}
	if isTriggerOrderType(m.OrderType) && m.TriggerPrice == 0 {
		return ErrInvalidOrder.Wrap("trigger_price must be > 0 for trigger orders")
	}
	if m.TimeInForce == perptypes.PostOnly && m.OrderType != perptypes.LimitOrder {
		return ErrInvalidOrder.Wrap("PostOnly only allowed on limit orders")
	}
	if m.TimeInForce == perptypes.GTT && m.Expiry <= 0 {
		return ErrInvalidOrder.Wrap("GTT orders require expiry > 0")
	}
	return nil
}
func (m *MsgCancelOrder) ValidateBasic() error     { return validAddr(m.Sender) }
func (m *MsgCancelAllOrders) ValidateBasic() error {
	if err := validAddr(m.Sender); err != nil {
		return err
	}
	switch m.Mode {
	case perptypes.ImmediateCancelAll, perptypes.ScheduledCancelAll, perptypes.AbortScheduledCancelAll:
		return nil
	}
	return ErrInvalidOrder.Wrapf("cancel-all mode=%d", m.Mode)
}
func (m *MsgModifyOrder) ValidateBasic() error {
	if err := validAddr(m.Sender); err != nil {
		return err
	}
	if m.NewBaseAmount == 0 || m.NewBaseAmount > perptypes.MaxOrderBaseAmount {
		return ErrInvalidOrder.Wrapf("new_base_amount out of range (got %d)", m.NewBaseAmount)
	}
	return nil
}
func (m *MsgUpdateParams) ValidateBasic() error {
	if err := validAddr(m.Authority); err != nil {
		return err
	}
	return m.Params.Validate()
}
