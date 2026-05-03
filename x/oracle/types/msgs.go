package types

import (
	sdk "github.com/cosmos/cosmos-sdk/types"
	sdkerrors "github.com/cosmos/cosmos-sdk/types/errors"
)

var (
	_ sdk.Msg = (*MsgBindOracleOperator)(nil)
	_ sdk.Msg = (*MsgUnbindOracleOperator)(nil)
	_ sdk.Msg = (*MsgAggregateOracleVotes)(nil)
	_ sdk.Msg = (*MsgInjectOracle)(nil)
	_ sdk.Msg = (*MsgAddOracleProvider)(nil)
	_ sdk.Msg = (*MsgUpdateOracleProvider)(nil)
	_ sdk.Msg = (*MsgSetAggregationMode)(nil)
	_ sdk.Msg = (*MsgUpdateParams)(nil)
)

func validAddr(s string) error {
	if _, err := sdk.AccAddressFromBech32(s); err != nil {
		return sdkerrors.ErrInvalidAddress.Wrap(err.Error())
	}
	return nil
}

func (m *MsgBindOracleOperator) ValidateBasic() error    { return validAddr(m.Sender) }
func (m *MsgUnbindOracleOperator) ValidateBasic() error  { return validAddr(m.Sender) }
func (m *MsgAggregateOracleVotes) ValidateBasic() error  { return validAddr(m.Authority) }
func (m *MsgInjectOracle) ValidateBasic() error          { return validAddr(m.Sender) }
func (m *MsgAddOracleProvider) ValidateBasic() error     { return validAddr(m.Authority) }
func (m *MsgUpdateOracleProvider) ValidateBasic() error  { return validAddr(m.Authority) }
func (m *MsgSetAggregationMode) ValidateBasic() error    { return validAddr(m.Authority) }
func (m *MsgUpdateParams) ValidateBasic() error {
	if err := validAddr(m.Authority); err != nil {
		return err
	}
	return m.Params.Validate()
}
