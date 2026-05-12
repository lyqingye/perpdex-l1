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

// validMarginChain enforces the canonical relationship between IMF /
// MF floors that every market must respect:
//
//	default_imf >= min_imf
//	maintenance_mf < default_imf
//	close_out_mf  < maintenance_mf
//
// Used by both MsgCreateMarket and MsgUpdateMarketDetails (and
// GenesisState.Validate via validateMarketDetailsStatics) so the rule
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

// validateMarketIndexForType checks that market_index falls in the
// canonical range for its declared market_type. Stateless: relies
// only on the chain-wide constants in `types/constants.go`.
//
// Note: the canonical constants are the absolute upper bound. The
// *current* Params range can be narrower (governance may shrink
// MaxPerpsMarketIndex over time). Drift between constants and Params
// is caught in msg_server.CreateMarket, which reads Params and
// rejects out-of-range indices before they reach the store.
func validateMarketIndexForType(idx, marketType uint32) error {
	if idx == perptypes.NilMarketIndex {
		return ErrMarketIndexExceed.Wrap("market_index must not equal NilMarketIndex")
	}
	switch marketType {
	case perptypes.MarketTypePerps:
		if idx > perptypes.MaxPerpsMarketIndex {
			return ErrMarketIndexExceed
		}
	case perptypes.MarketTypeSpot:
		if idx < perptypes.MinSpotMarketIndex || idx > perptypes.MaxSpotMarketIndex {
			return ErrMarketIndexExceed
		}
	default:
		return ErrInvalidMarket.Wrapf("market_type=%d", marketType)
	}
	return nil
}

// validateMarketStatics checks every field on the static Market record
// that depends solely on the msg payload (no ctx, no store). It does
// NOT check `Status`: callers know whether they expect a create-time
// "must be ACTIVE" or a genesis "may be ACTIVE or EXPIRED".
//
// Shared by MsgCreateMarket.ValidateBasic (+ status==ACTIVE wrapper)
// and GenesisState.Validate (+ status∈{ACTIVE,EXPIRED} wrapper).
func validateMarketStatics(m Market) error {
	if err := validateMarketIndexForType(m.MarketIndex, m.MarketType); err != nil {
		return err
	}
	if m.QuoteAssetId == perptypes.NilAssetIndex {
		return ErrInvalidMarket.Wrap("quote_asset_id must be set")
	}
	if m.MarketType == perptypes.MarketTypeSpot {
		if m.BaseAssetId == perptypes.NilAssetIndex {
			return ErrInvalidMarket.Wrap("spot market base_asset_id must be set")
		}
		if m.BaseAssetId == m.QuoteAssetId {
			return ErrInvalidMarket.Wrap("spot market base_asset_id must differ from quote_asset_id")
		}
	}
	if uint64(m.TakerFee) >= perptypes.FeeTick {
		return ErrInvalidParams.Wrapf("taker_fee=%d >= FeeTick=%d", m.TakerFee, perptypes.FeeTick)
	}
	if uint64(m.MakerFee) >= perptypes.FeeTick {
		return ErrInvalidParams.Wrapf("maker_fee=%d >= FeeTick=%d", m.MakerFee, perptypes.FeeTick)
	}
	if uint64(m.LiquidationFee) >= perptypes.FeeTick {
		return ErrInvalidParams.Wrapf("liquidation_fee=%d >= FeeTick=%d", m.LiquidationFee, perptypes.FeeTick)
	}
	if m.MinBaseAmount == 0 {
		return ErrInvalidParams.Wrap("min_base_amount must be > 0")
	}
	if m.MinQuoteAmount == 0 {
		return ErrInvalidParams.Wrap("min_quote_amount must be > 0")
	}
	if m.OrderQuoteLimit < 0 {
		return ErrInvalidParams.Wrap("order_quote_limit must be >= 0")
	}
	if m.SizeExtensionMultiplier <= 0 {
		return ErrInvalidParams.Wrap("size_extension_multiplier must be > 0")
	}
	if m.QuoteExtensionMultiplier <= 0 {
		return ErrInvalidParams.Wrap("quote_extension_multiplier must be > 0")
	}
	if m.ExpiryTimestamp < 0 {
		return ErrInvalidMarket.Wrap("expiry_timestamp must be >= 0")
	}
	return nil
}

// validateMarketDetailsStatics covers every MarketDetails field that
// is checkable without ctx/store. Shared by MsgCreateMarket,
// MsgUpdateMarketDetails and GenesisState.Validate.
func validateMarketDetailsStatics(d MarketDetails) error {
	if d.MinInitialMarginFraction == 0 {
		return ErrInvalidParams.Wrap("min_imf must be > 0")
	}
	if d.DefaultInitialMarginFraction > uint32(perptypes.MarginTick) ||
		d.MinInitialMarginFraction > uint32(perptypes.MarginTick) ||
		d.MaintenanceMarginFraction > uint32(perptypes.MarginTick) ||
		d.CloseOutMarginFraction > uint32(perptypes.MarginTick) {
		return ErrInvalidParams.Wrapf(
			"margin fraction above MarginTick=%d", perptypes.MarginTick,
		)
	}
	if !validMarginChain(
		d.DefaultInitialMarginFraction,
		d.MinInitialMarginFraction,
		d.MaintenanceMarginFraction,
		d.CloseOutMarginFraction,
	) {
		return ErrInvalidMarginChain
	}
	if d.FundingClampSmall > d.FundingClampBig {
		return ErrInvalidParams.Wrap("funding_clamp_small must be <= funding_clamp_big")
	}
	// interest_rate is expressed in FundingRateTick units. Anything
	// above the tick is almost certainly a unit-confusion bug; reject
	// rather than silently misprice funding.
	if int64(d.InterestRate) > perptypes.FundingRateTick {
		return ErrInvalidParams.Wrapf(
			"interest_rate=%d above FundingRateTick=%d",
			d.InterestRate, perptypes.FundingRateTick,
		)
	}
	return nil
}

// validateMarketDetailsInit covers the create-time-only invariants on
// MarketDetails. The runtime fields (mark price, OI, premium sums,
// etc.) MUST start at zero so a malicious / mistaken gov proposal
// cannot seed the market with poisoned state. The nonce range MUST
// equal the canonical bounds; AllocateNonce advances them from there.
//
// Used by MsgCreateMarket.ValidateBasic. Genesis is a separate path
// (operators can pre-seed a recovered chain mid-flight); it has its
// own laxer validation that re-uses validateMarketDetailsStatics.
func validateMarketDetailsInit(d MarketDetails) error {
	if d.AskNonce != perptypes.FirstAskNonce {
		return ErrNonceExhausted.Wrapf(
			"ask_nonce=%d must equal FirstAskNonce=%d at creation",
			d.AskNonce, perptypes.FirstAskNonce,
		)
	}
	if d.BidNonce != perptypes.FirstBidNonce {
		return ErrNonceExhausted.Wrapf(
			"bid_nonce=%d must equal FirstBidNonce=%d at creation",
			d.BidNonce, perptypes.FirstBidNonce,
		)
	}
	if d.OpenInterest != 0 {
		return ErrInvalidParams.Wrap("open_interest must be 0 at creation")
	}
	if d.AggregatePremiumSum != 0 {
		return ErrInvalidParams.Wrap("aggregate_premium_sum must be 0 at creation")
	}
	if d.TotalOrderCount != 0 {
		return ErrInvalidParams.Wrap("total_order_count must be 0 at creation")
	}
	if d.TotalPremiumSamples != 0 {
		return ErrInvalidParams.Wrap("total_premium_samples must be 0 at creation")
	}
	if d.LastUpdatedTimestamp != 0 {
		return ErrInvalidParams.Wrap("last_updated_timestamp must be 0 at creation")
	}
	if d.MarkPrice != 0 || d.IndexPrice != 0 ||
		d.ImpactBidPrice != 0 || d.ImpactAskPrice != 0 || d.ImpactPrice != 0 {
		return ErrInvalidParams.Wrap("mark/index/impact prices must be 0 at creation")
	}
	if !d.FundingRatePrefixSum.IsNil() && !d.FundingRatePrefixSum.IsZero() {
		return ErrInvalidParams.Wrap("funding_rate_prefix_sum must be 0 at creation")
	}
	return nil
}

func (m *MsgCreateMarket) ValidateBasic() error {
	if err := validAuth(m.Authority); err != nil {
		return err
	}
	if m.Market.MarketIndex != m.MarketDetails.MarketIndex {
		return ErrInvalidMarket.Wrap("market and details index mismatch")
	}
	// Create-time status must be ACTIVE. EXPIRED markets only ever
	// arise through the keeper.expireMarket helper.
	if m.Market.Status != perptypes.MarketStatusActive {
		return ErrInvalidMarket.Wrapf("status=%d must be ACTIVE at creation", m.Market.Status)
	}
	if err := validateMarketStatics(m.Market); err != nil {
		return err
	}
	if err := validateMarketDetailsStatics(m.MarketDetails); err != nil {
		return err
	}
	if err := validateMarketDetailsInit(m.MarketDetails); err != nil {
		return err
	}
	return nil
}

func (m *MsgUpdateMarket) ValidateBasic() error {
	if err := validAuth(m.Authority); err != nil {
		return err
	}
	if m.MarketIndex == perptypes.NilMarketIndex {
		return ErrMarketIndexExceed.Wrap("market_index must not equal NilMarketIndex")
	}
	if m.NewStatus != perptypes.MarketStatusActive &&
		m.NewStatus != perptypes.MarketStatusExpired {
		return ErrInvalidMarket.Wrapf("new_status=%d", m.NewStatus)
	}
	if uint64(m.NewTakerFee) >= perptypes.FeeTick {
		return ErrInvalidParams.Wrapf(
			"new_taker_fee=%d >= FeeTick=%d", m.NewTakerFee, perptypes.FeeTick,
		)
	}
	if uint64(m.NewMakerFee) >= perptypes.FeeTick {
		return ErrInvalidParams.Wrapf(
			"new_maker_fee=%d >= FeeTick=%d", m.NewMakerFee, perptypes.FeeTick,
		)
	}
	if uint64(m.NewLiquidationFee) >= perptypes.FeeTick {
		return ErrInvalidParams.Wrapf(
			"new_liquidation_fee=%d >= FeeTick=%d", m.NewLiquidationFee, perptypes.FeeTick,
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
	if m.NewExpiryTimestamp < 0 {
		return ErrInvalidMarket.Wrap("new_expiry_timestamp must be >= 0")
	}
	// EXPIRED + a finite future expiry is self-contradictory: either
	// the operator wants to delist now (status=EXPIRED, expiry
	// unchanged) or to reschedule the future expiry (status=ACTIVE,
	// expiry>0). Reject the contradictory combination at the basic
	// layer so it never reaches the handler.
	if m.NewStatus == perptypes.MarketStatusExpired && m.NewExpiryTimestamp > 0 {
		return ErrInvalidMarket.Wrap("new_status=EXPIRED is incompatible with a future new_expiry_timestamp")
	}
	return nil
}

func (m *MsgUpdateMarketDetails) ValidateBasic() error {
	if err := validAuth(m.Authority); err != nil {
		return err
	}
	if m.MarketIndex == perptypes.NilMarketIndex {
		return ErrMarketIndexExceed.Wrap("market_index must not equal NilMarketIndex")
	}
	d := MarketDetails{
		DefaultInitialMarginFraction: m.NewDefaultImf,
		MinInitialMarginFraction:     m.NewMinImf,
		MaintenanceMarginFraction:    m.NewMaintenanceMf,
		CloseOutMarginFraction:       m.NewCloseOutMf,
		FundingClampSmall:            m.NewFundingClampSmall,
		FundingClampBig:              m.NewFundingClampBig,
		InterestRate:                 m.NewInterestRate,
	}
	if err := validateMarketDetailsStatics(d); err != nil {
		return err
	}
	// OpenInterestLimit upper bound: even though the field is uint64,
	// values above MaxOrderBaseAmount are nonsensical because no
	// single trade could ever push OI beyond per-order limits.
	if m.NewOpenInterestLimit > perptypes.MaxOrderBaseAmount {
		return ErrInvalidParams.Wrapf(
			"new_open_interest_limit=%d above MaxOrderBaseAmount=%d",
			m.NewOpenInterestLimit, perptypes.MaxOrderBaseAmount,
		)
	}
	return nil
}

func (m *MsgUpdateParams) ValidateBasic() error {
	if err := validAuth(m.Authority); err != nil {
		return err
	}
	return m.Params.Validate()
}
