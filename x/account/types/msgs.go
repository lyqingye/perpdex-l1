package types

import (
	sdk "github.com/cosmos/cosmos-sdk/types"
	sdkerrors "github.com/cosmos/cosmos-sdk/types/errors"

	perptypes "github.com/perpdex/perpdex-l1/types"
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

// validRoute reports whether the route belongs to the closed
// {Perps, Spot} set. Route validity is stateless, so we gate it
// here instead of repeating the switch in every msg_server handler.
func validRoute(r uint32) bool {
	return r == perptypes.RouteTypePerps || r == perptypes.RouteTypeSpot
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
	if !validRoute(m.RouteType) {
		return ErrInvalidRoute.Wrapf("route_type=%d", m.RouteType)
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
	if !validRoute(m.RouteType) {
		return ErrInvalidRoute.Wrapf("route_type=%d", m.RouteType)
	}
	return nil
}

func (m *MsgCreateSubAccount) ValidateBasic() error { return mustValidAddr(m.Sender) }

func (m *MsgUpdateAccountConfig) ValidateBasic() error {
	if err := mustValidAddr(m.Sender); err != nil {
		return err
	}
	if m.NewTradingMode != perptypes.AccountTradingModeSimple &&
		m.NewTradingMode != perptypes.AccountTradingModeUnified {
		return ErrInvalidTradingMode.Wrapf("new_trading_mode=%d", m.NewTradingMode)
	}
	return nil
}

func (m *MsgUpdateAccountAssetConfig) ValidateBasic() error {
	if err := mustValidAddr(m.Sender); err != nil {
		return err
	}
	if m.NewMarginMode != perptypes.MarginModeDisabled &&
		m.NewMarginMode != perptypes.MarginModeEnabled {
		return ErrInvalidMarginMode.Wrapf("new_margin_mode=%d", m.NewMarginMode)
	}
	return nil
}

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
	if m.Action != perptypes.AddMargin && m.Action != perptypes.RemoveMargin {
		return ErrInvalidMarginAction.Wrapf("action=%d", m.Action)
	}
	return nil
}

func (m *MsgUpdateLeverage) ValidateBasic() error {
	if err := mustValidAddr(m.Sender); err != nil {
		return err
	}
	if m.NewMarginMode != perptypes.CrossMargin && m.NewMarginMode != perptypes.IsolatedMargin {
		return ErrInvalidMarginMode.Wrapf("new_margin_mode=%d", m.NewMarginMode)
	}
	// Upper bound is a chain-constant; the market-specific floor is
	// still enforced in msg_server (needs MarketKeeper lookup).
	if m.NewInitialMarginFraction > uint32(perptypes.MarginTick) {
		return ErrInvalidParams.Wrapf(
			"new_initial_margin_fraction=%d exceeds MarginTick=%d",
			m.NewInitialMarginFraction, perptypes.MarginTick,
		)
	}
	return nil
}

func (m *MsgUpdateParams) ValidateBasic() error {
	if err := mustValidAddr(m.Authority); err != nil {
		return err
	}
	return m.Params.Validate()
}

func (m *MsgCreatePublicPool) ValidateBasic() error {
	if err := mustValidAddr(m.Sender); err != nil {
		return err
	}
	if m.InitialTotalShares == 0 {
		return ErrInvalidParams.Wrap("initial_total_shares must be > 0")
	}
	// MsgCreatePublicPool only spawns regular PUBLIC_POOL pools. The
	// canonical IF pool is genesis-only and is mutated via
	// MsgUpdatePublicPool / MsgStrategyTransfer.
	if m.AccountType != perptypes.PublicPoolAccountType {
		return ErrInvalidAccountType.Wrapf(
			"account_type must be PUBLIC_POOL(%d); IF pool is genesis-only",
			perptypes.PublicPoolAccountType,
		)
	}
	if m.OperatorFee >= uint32(perptypes.FeeTick) {
		return ErrInvalidParams.Wrapf(
			"operator_fee must be < FeeTick(%d)", perptypes.FeeTick,
		)
	}
	if m.MinOperatorShareRate > perptypes.ShareTick {
		return ErrInvalidParams.Wrapf(
			"min_operator_share_rate must be <= ShareTick(%d)", perptypes.ShareTick,
		)
	}
	return nil
}

func (m *MsgUpdatePublicPool) ValidateBasic() error {
	if err := mustValidAddr(m.Sender); err != nil {
		return err
	}
	if m.NewStatus != perptypes.PublicPoolStatusActive &&
		m.NewStatus != perptypes.PublicPoolStatusFrozen {
		return ErrInvalidPoolUpdate.Wrapf("unknown status %d", m.NewStatus)
	}
	if m.NewMinOperatorShareRate > perptypes.ShareTick {
		return ErrInvalidParams.Wrapf(
			"min_operator_share_rate must be <= ShareTick(%d)", perptypes.ShareTick,
		)
	}
	return nil
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
	if m.FromStrategy >= uint32(perptypes.NbStrategies) ||
		m.ToStrategy >= uint32(perptypes.NbStrategies) {
		return ErrInvalidStrategyIdx.Wrapf(
			"from=%d to=%d nb_strategies=%d", m.FromStrategy, m.ToStrategy, perptypes.NbStrategies,
		)
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
