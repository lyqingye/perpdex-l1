package types

import (
	sdk "github.com/cosmos/cosmos-sdk/types"
	sdkerrors "github.com/cosmos/cosmos-sdk/types/errors"

	perptypes "github.com/perpdex/perpdex-l1/types"
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

// validMarginChain enforces the canonical relationship between IMF / MF
// floors that every market must respect:
//
//	default_imf >= min_imf
//	maintenance_mf < default_imf
//	close_out_mf  < maintenance_mf
//
// Used by both MsgCreateMarket and MsgUpdateMarketDetails so the rule
// has a single home.
func validMarginChain(defaultImf, minImf, maintenanceMf, closeOutMf uint32) bool {
	if defaultImf < minImf {
		return false
	}
	if maintenanceMf >= defaultImf {
		return false
	}
	if closeOutMf >= maintenanceMf {
		return false
	}
	return true
}

func (m *MsgCreateMarket) ValidateBasic() error {
	if err := validAuth(m.Authority); err != nil {
		return err
	}
	market := m.Market
	details := m.MarketDetails
	if market.MarketIndex != details.MarketIndex {
		return ErrInvalidMarket.Wrap("market and details index mismatch")
	}
	switch market.MarketType {
	case perptypes.MarketTypePerps:
		if market.MarketIndex > perptypes.MaxPerpsMarketIndex {
			return ErrMarketIndexExceed
		}
	case perptypes.MarketTypeSpot:
		if market.MarketIndex < perptypes.MinSpotMarketIndex || market.MarketIndex > perptypes.MaxSpotMarketIndex {
			return ErrMarketIndexExceed
		}
	default:
		return ErrInvalidMarket.Wrapf("market_type=%d", market.MarketType)
	}
	if !validMarginChain(
		details.DefaultInitialMarginFraction,
		details.MinInitialMarginFraction,
		details.MaintenanceMarginFraction,
		details.CloseOutMarginFraction,
	) {
		return ErrInvalidMarginChain
	}
	return nil
}

func (m *MsgUpdateMarket) ValidateBasic() error {
	if err := validAuth(m.Authority); err != nil {
		return err
	}
	if m.NewStatus != perptypes.MarketStatusActive &&
		m.NewStatus != perptypes.MarketStatusExpired {
		return ErrInvalidMarket.Wrapf("new_status=%d", m.NewStatus)
	}
	if uint64(m.NewTakerFee) >= uint64(perptypes.FeeTick) {
		return ErrInvalidParams.Wrapf(
			"new_taker_fee=%d >= FeeTick=%d", m.NewTakerFee, perptypes.FeeTick,
		)
	}
	if uint64(m.NewMakerFee) >= uint64(perptypes.FeeTick) {
		return ErrInvalidParams.Wrapf(
			"new_maker_fee=%d >= FeeTick=%d", m.NewMakerFee, perptypes.FeeTick,
		)
	}
	if m.NewMinBaseAmount == 0 {
		return ErrInvalidParams.Wrap("new_min_base_amount must be > 0")
	}
	if m.NewMinQuoteAmount == 0 {
		return ErrInvalidParams.Wrap("new_min_quote_amount must be > 0")
	}
	if m.NewOrderQuoteLimit < 0 {
		return ErrInvalidParams.Wrap("new_order_quote_limit must be >= 0")
	}
	return nil
}

func (m *MsgUpdateMarketDetails) ValidateBasic() error {
	if err := validAuth(m.Authority); err != nil {
		return err
	}
	if m.NewMinImf == 0 {
		return ErrInvalidParams.Wrap("new_min_imf must be > 0")
	}
	if m.NewDefaultImf > uint32(perptypes.MarginTick) ||
		m.NewMinImf > uint32(perptypes.MarginTick) ||
		m.NewMaintenanceMf > uint32(perptypes.MarginTick) ||
		m.NewCloseOutMf > uint32(perptypes.MarginTick) {
		return ErrInvalidParams.Wrapf(
			"margin fraction above MarginTick=%d", perptypes.MarginTick,
		)
	}
	if !validMarginChain(m.NewDefaultImf, m.NewMinImf, m.NewMaintenanceMf, m.NewCloseOutMf) {
		return ErrInvalidMarginChain
	}
	if m.NewFundingClampSmall > m.NewFundingClampBig {
		return ErrInvalidParams.Wrap("funding_clamp_small must be <= funding_clamp_big")
	}
	return nil
}

func (m *MsgUpdateParams) ValidateBasic() error {
	if err := validAuth(m.Authority); err != nil {
		return err
	}
	return m.Params.Validate()
}
