package types

import (
	sdk "github.com/cosmos/cosmos-sdk/types"
	sdkerrors "github.com/cosmos/cosmos-sdk/types/errors"
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

func (m *MsgCreateOrder) ValidateBasic() error {
	if err := validAddr(m.Sender); err != nil {
		return err
	}
	if m.BaseAmount == 0 {
		return ErrInvalidOrder.Wrap("base_amount must be > 0")
	}
	return nil
}
func (m *MsgCancelOrder) ValidateBasic() error     { return validAddr(m.Sender) }
func (m *MsgCancelAllOrders) ValidateBasic() error { return validAddr(m.Sender) }
func (m *MsgModifyOrder) ValidateBasic() error     { return validAddr(m.Sender) }
func (m *MsgUpdateParams) ValidateBasic() error {
	if err := validAddr(m.Authority); err != nil {
		return err
	}
	return m.Params.Validate()
}
