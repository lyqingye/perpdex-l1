package types

import (
	sdk "github.com/cosmos/cosmos-sdk/types"
	sdkerrors "github.com/cosmos/cosmos-sdk/types/errors"
)

var (
	_ sdk.Msg = (*MsgCreateMarket)(nil)
	_ sdk.Msg = (*MsgUpdateMarket)(nil)
	_ sdk.Msg = (*MsgUpdateMarketDetails)(nil)
	_ sdk.Msg = (*MsgUpdateParams)(nil)
)

func validAuth(a string) error {
	if _, err := sdk.AccAddressFromBech32(a); err != nil {
		return sdkerrors.ErrInvalidAddress.Wrap(err.Error())
	}
	return nil
}

func (m *MsgCreateMarket) ValidateBasic() error           { return validAuth(m.Authority) }
func (m *MsgUpdateMarket) ValidateBasic() error           { return validAuth(m.Authority) }
func (m *MsgUpdateMarketDetails) ValidateBasic() error    { return validAuth(m.Authority) }
func (m *MsgUpdateParams) ValidateBasic() error {
	if err := validAuth(m.Authority); err != nil {
		return err
	}
	return m.Params.Validate()
}
