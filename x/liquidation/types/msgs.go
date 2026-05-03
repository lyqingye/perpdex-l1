package types

import (
	sdk "github.com/cosmos/cosmos-sdk/types"
	sdkerrors "github.com/cosmos/cosmos-sdk/types/errors"
)

var (
	_ sdk.Msg = (*MsgLiquidate)(nil)
	_ sdk.Msg = (*MsgDeleverage)(nil)
)

func (m *MsgLiquidate) ValidateBasic() error {
	if _, err := sdk.AccAddressFromBech32(m.Sender); err != nil {
		return sdkerrors.ErrInvalidAddress.Wrap(err.Error())
	}
	if m.BaseAmount == 0 {
		return ErrInvalidParams.Wrap("base_amount must be > 0")
	}
	return nil
}

func (m *MsgDeleverage) ValidateBasic() error {
	if _, err := sdk.AccAddressFromBech32(m.Sender); err != nil {
		return sdkerrors.ErrInvalidAddress.Wrap(err.Error())
	}
	return nil
}
