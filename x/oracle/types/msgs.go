package types

import (
	sdk "github.com/cosmos/cosmos-sdk/types"
	sdkerrors "github.com/cosmos/cosmos-sdk/types/errors"
)

var _ sdk.Msg = (*MsgUpdateParams)(nil)

func validAddr(s string) error {
	if _, err := sdk.AccAddressFromBech32(s); err != nil {
		return sdkerrors.ErrInvalidAddress.Wrap(err.Error())
	}
	return nil
}

func (m *MsgUpdateParams) ValidateBasic() error {
	if err := validAddr(m.Authority); err != nil {
		return err
	}
	return m.Params.Validate()
}
