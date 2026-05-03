package types

import (
	sdk "github.com/cosmos/cosmos-sdk/types"
	sdkerrors "github.com/cosmos/cosmos-sdk/types/errors"

	perptypes "github.com/perpdex/perpdex-l1/types"
)

var (
	_ sdk.Msg = (*MsgRegisterAsset)(nil)
	_ sdk.Msg = (*MsgUpdateAsset)(nil)
	_ sdk.Msg = (*MsgUpdateParams)(nil)
)

func (m *MsgRegisterAsset) ValidateBasic() error {
	if _, err := sdk.AccAddressFromBech32(m.Authority); err != nil {
		return sdkerrors.ErrInvalidAddress.Wrapf("authority: %s", err)
	}
	if m.Denom == "" {
		return ErrInvalidAssetParams.Wrap("denom must not be empty")
	}
	if m.ExtensionMultiplier == 0 {
		return ErrInvalidAssetParams.Wrap("extension_multiplier must be > 0")
	}
	if m.MinTransferAmount == 0 {
		return ErrInvalidAssetParams.Wrap("min_transfer_amount must be > 0")
	}
	if m.MinWithdrawalAmount == 0 {
		return ErrInvalidAssetParams.Wrap("min_withdrawal_amount must be > 0")
	}
	if m.MarginMode != perptypes.MarginModeDisabled && m.MarginMode != perptypes.MarginModeEnabled {
		return ErrInvalidAssetParams.Wrap("margin_mode out of range")
	}
	return nil
}

func (m *MsgUpdateAsset) ValidateBasic() error {
	if _, err := sdk.AccAddressFromBech32(m.Authority); err != nil {
		return sdkerrors.ErrInvalidAddress.Wrapf("authority: %s", err)
	}
	if m.MinTransferAmount == 0 {
		return ErrInvalidAssetParams.Wrap("min_transfer_amount must be > 0")
	}
	if m.MinWithdrawalAmount == 0 {
		return ErrInvalidAssetParams.Wrap("min_withdrawal_amount must be > 0")
	}
	return nil
}

func (m *MsgUpdateParams) ValidateBasic() error {
	if _, err := sdk.AccAddressFromBech32(m.Authority); err != nil {
		return sdkerrors.ErrInvalidAddress.Wrapf("authority: %s", err)
	}
	return m.Params.Validate()
}
