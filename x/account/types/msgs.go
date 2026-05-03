package types

import (
	sdk "github.com/cosmos/cosmos-sdk/types"
	sdkerrors "github.com/cosmos/cosmos-sdk/types/errors"
)

var (
	_ sdk.Msg = (*MsgDeposit)(nil)
	_ sdk.Msg = (*MsgWithdraw)(nil)
	_ sdk.Msg = (*MsgCreateSubAccount)(nil)
	_ sdk.Msg = (*MsgUpdateAccountConfig)(nil)
	_ sdk.Msg = (*MsgUpdateAccountAssetConfig)(nil)
	_ sdk.Msg = (*MsgTransfer)(nil)
	_ sdk.Msg = (*MsgUpdateMargin)(nil)
	_ sdk.Msg = (*MsgUpdateLeverage)(nil)
	_ sdk.Msg = (*MsgUpdateParams)(nil)
	_ sdk.Msg = (*MsgCreatePublicPool)(nil)
	_ sdk.Msg = (*MsgUpdatePublicPool)(nil)
	_ sdk.Msg = (*MsgMintShares)(nil)
	_ sdk.Msg = (*MsgBurnShares)(nil)
	_ sdk.Msg = (*MsgStrategyTransfer)(nil)
	_ sdk.Msg = (*MsgForceBurnShares)(nil)
)

func mustValidAddr(s string) error {
	if s == "" {
		return sdkerrors.ErrInvalidAddress.Wrap("address must not be empty")
	}
	if _, err := sdk.AccAddressFromBech32(s); err != nil {
		return sdkerrors.ErrInvalidAddress.Wrap(err.Error())
	}
	return nil
}

func (m *MsgDeposit) ValidateBasic() error {
	if err := mustValidAddr(m.Sender); err != nil {
		return err
	}
	if m.Beneficiary != "" {
		if err := mustValidAddr(m.Beneficiary); err != nil {
			return err
		}
	}
	if m.Amount == 0 {
		return ErrAmountTooSmall
	}
	return nil
}

func (m *MsgWithdraw) ValidateBasic() error {
	if err := mustValidAddr(m.Sender); err != nil {
		return err
	}
	if m.DestinationAddress != "" {
		if err := mustValidAddr(m.DestinationAddress); err != nil {
			return err
		}
	}
	if m.Amount == 0 {
		return ErrAmountTooSmall
	}
	return nil
}

func (m *MsgCreateSubAccount) ValidateBasic() error { return mustValidAddr(m.Sender) }

func (m *MsgUpdateAccountConfig) ValidateBasic() error { return mustValidAddr(m.Sender) }

func (m *MsgUpdateAccountAssetConfig) ValidateBasic() error { return mustValidAddr(m.Sender) }

func (m *MsgTransfer) ValidateBasic() error {
	if err := mustValidAddr(m.Sender); err != nil {
		return err
	}
	if m.Amount == 0 {
		return ErrAmountTooSmall
	}
	if m.FromAccountIndex == m.ToAccountIndex {
		return ErrInvalidParams.Wrap("from and to accounts must differ")
	}
	return nil
}

func (m *MsgUpdateMargin) ValidateBasic() error {
	if err := mustValidAddr(m.Sender); err != nil {
		return err
	}
	if m.Amount.IsNil() {
		return ErrInvalidParams.Wrap("amount must be set")
	}
	if !m.Amount.IsPositive() {
		return ErrInvalidParams.Wrap("amount must be positive")
	}
	return nil
}

func (m *MsgUpdateLeverage) ValidateBasic() error { return mustValidAddr(m.Sender) }

func (m *MsgUpdateParams) ValidateBasic() error {
	if err := mustValidAddr(m.Authority); err != nil {
		return err
	}
	return m.Params.Validate()
}

// ---------- public pool msgs ----------

func (m *MsgCreatePublicPool) ValidateBasic() error {
	if err := mustValidAddr(m.Sender); err != nil {
		return err
	}
	if m.InitialTotalShares == 0 {
		return ErrInvalidParams.Wrap("initial_total_shares must be > 0")
	}
	return nil
}

func (m *MsgUpdatePublicPool) ValidateBasic() error {
	return mustValidAddr(m.Sender)
}

func (m *MsgMintShares) ValidateBasic() error {
	if err := mustValidAddr(m.Sender); err != nil {
		return err
	}
	if m.PrincipalAmount == 0 {
		return ErrAmountTooSmall
	}
	return nil
}

func (m *MsgBurnShares) ValidateBasic() error {
	if err := mustValidAddr(m.Sender); err != nil {
		return err
	}
	if m.ShareAmount.IsNil() || !m.ShareAmount.IsPositive() {
		return ErrInvalidParams.Wrap("share_amount must be positive")
	}
	return nil
}

func (m *MsgStrategyTransfer) ValidateBasic() error {
	if err := mustValidAddr(m.Sender); err != nil {
		return err
	}
	if m.FromStrategy == m.ToStrategy {
		return ErrInvalidParams.Wrap("from and to strategy must differ")
	}
	if m.Amount.IsNil() || !m.Amount.IsPositive() {
		return ErrInvalidParams.Wrap("amount must be positive")
	}
	return nil
}

func (m *MsgForceBurnShares) ValidateBasic() error {
	if err := mustValidAddr(m.Authority); err != nil {
		return err
	}
	if m.ShareAmount.IsNil() || !m.ShareAmount.IsPositive() {
		return ErrInvalidParams.Wrap("share_amount must be positive")
	}
	return nil
}
