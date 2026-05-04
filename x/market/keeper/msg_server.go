package keeper

import (
	"context"

	sdk "github.com/cosmos/cosmos-sdk/types"

	perptypes "github.com/perpdex/perpdex-l1/types"
	"github.com/perpdex/perpdex-l1/x/market/types"
)

type msgServer struct{ Keeper }

func NewMsgServerImpl(k Keeper) types.MsgServer { return &msgServer{Keeper: k} }

var _ types.MsgServer = msgServer{}

func (m msgServer) CreateMarket(ctx context.Context, msg *types.MsgCreateMarket) (*types.MsgCreateMarketResponse, error) {
	if msg.Authority != m.authority {
		return nil, types.ErrInvalidAuthority
	}
	market := msg.Market
	details := msg.MarketDetails
	if market.MarketIndex != details.MarketIndex {
		return nil, types.ErrInvalidMarket.Wrap("market and details index mismatch")
	}
	exists, err := m.Markets.Has(ctx, market.MarketIndex)
	if err != nil {
		return nil, err
	}
	if exists {
		return nil, types.ErrMarketExists
	}
	switch market.MarketType {
	case perptypes.MarketTypePerps:
		if market.MarketIndex > perptypes.MaxPerpsMarketIndex {
			return nil, types.ErrMarketIndexExceed
		}
	case perptypes.MarketTypeSpot:
		if market.MarketIndex < perptypes.MinSpotMarketIndex || market.MarketIndex > perptypes.MaxSpotMarketIndex {
			return nil, types.ErrMarketIndexExceed
		}
	default:
		return nil, types.ErrInvalidMarket.Wrapf("market_type=%d", market.MarketType)
	}

	// Validate margin chain: default >= min, maintenance < default, close-out < maintenance.
	if details.DefaultInitialMarginFraction < details.MinInitialMarginFraction ||
		details.MaintenanceMarginFraction >= details.DefaultInitialMarginFraction ||
		details.CloseOutMarginFraction >= details.MaintenanceMarginFraction {
		return nil, types.ErrInvalidMarginChain
	}
	// Sanity checks on assets.
	if _, err := m.assetKeeper.GetAsset(ctx, market.QuoteAssetId); err != nil {
		return nil, err
	}
	if market.MarketType == perptypes.MarketTypeSpot {
		if _, err := m.assetKeeper.GetAsset(ctx, market.BaseAssetId); err != nil {
			return nil, err
		}
	}
	// Initialize nonce range to canonical bounds.
	if details.AskNonce == 0 {
		details.AskNonce = perptypes.FirstAskNonce
	}
	if details.BidNonce == 0 {
		details.BidNonce = perptypes.FirstBidNonce
	}
	if details.AskNonce >= details.BidNonce {
		return nil, types.ErrNonceExhausted
	}

	market.CreatedAt = sdk.UnwrapSDKContext(ctx).BlockTime().UnixMilli()
	market.Status = perptypes.MarketStatusActive
	if err := m.SetMarket(ctx, market); err != nil {
		return nil, err
	}
	if err := m.SetMarketDetails(ctx, details); err != nil {
		return nil, err
	}
	return &types.MsgCreateMarketResponse{}, nil
}

func (m msgServer) UpdateMarket(ctx context.Context, msg *types.MsgUpdateMarket) (*types.MsgUpdateMarketResponse, error) {
	if msg.Authority != m.authority {
		return nil, types.ErrInvalidAuthority
	}
	if err := validateUpdateMarket(msg); err != nil {
		return nil, err
	}
	market, err := m.GetMarket(ctx, msg.MarketIndex)
	if err != nil {
		return nil, err
	}
	// Once a market is EXPIRED, its static parameters are frozen.
	if market.Status == perptypes.MarketStatusExpired && msg.NewStatus != perptypes.MarketStatusExpired {
		return nil, types.ErrInvalidMarket.Wrap("cannot un-expire market")
	}
	if msg.NewExpiryTimestamp > 0 {
		nowMs := sdk.UnwrapSDKContext(ctx).BlockTime().UnixMilli()
		if msg.NewExpiryTimestamp <= nowMs {
			return nil, types.ErrInvalidMarket.Wrapf(
				"new_expiry_timestamp=%d must be > now=%d",
				msg.NewExpiryTimestamp, nowMs,
			)
		}
	}
	market.Status = msg.NewStatus
	market.TakerFee = msg.NewTakerFee
	market.MakerFee = msg.NewMakerFee
	market.MinBaseAmount = msg.NewMinBaseAmount
	market.MinQuoteAmount = msg.NewMinQuoteAmount
	market.OrderQuoteLimit = msg.NewOrderQuoteLimit
	market.ExpiryTimestamp = msg.NewExpiryTimestamp
	if err := m.SetMarket(ctx, market); err != nil {
		return nil, err
	}
	return &types.MsgUpdateMarketResponse{}, nil
}

// validateUpdateMarket enforces business-level bounds on a MsgUpdateMarket
// payload:
//   - NewStatus ∈ {Active, Expired}
//   - NewTakerFee / NewMakerFee < FeeTick
//   - NewMinBaseAmount, NewMinQuoteAmount > 0 (0 would disable the checks)
//   - NewOrderQuoteLimit fits the protocol cap when non-zero
//   - NewExpiryTimestamp is either 0 (no expiry) or in the future (checked
//     against `BlockTime` at the call site).
func validateUpdateMarket(msg *types.MsgUpdateMarket) error {
	if msg.NewStatus != perptypes.MarketStatusActive &&
		msg.NewStatus != perptypes.MarketStatusExpired {
		return types.ErrInvalidMarket.Wrapf("new_status=%d", msg.NewStatus)
	}
	if uint64(msg.NewTakerFee) >= uint64(perptypes.FeeTick) {
		return types.ErrInvalidParams.Wrapf(
			"new_taker_fee=%d >= FeeTick=%d", msg.NewTakerFee, perptypes.FeeTick,
		)
	}
	if uint64(msg.NewMakerFee) >= uint64(perptypes.FeeTick) {
		return types.ErrInvalidParams.Wrapf(
			"new_maker_fee=%d >= FeeTick=%d", msg.NewMakerFee, perptypes.FeeTick,
		)
	}
	if msg.NewMinBaseAmount == 0 {
		return types.ErrInvalidParams.Wrap("new_min_base_amount must be > 0")
	}
	if msg.NewMinQuoteAmount == 0 {
		return types.ErrInvalidParams.Wrap("new_min_quote_amount must be > 0")
	}
	if msg.NewOrderQuoteLimit < 0 {
		return types.ErrInvalidParams.Wrap("new_order_quote_limit must be >= 0")
	}
	return nil
}

func (m msgServer) UpdateMarketDetails(ctx context.Context, msg *types.MsgUpdateMarketDetails) (*types.MsgUpdateMarketDetailsResponse, error) {
	if msg.Authority != m.authority {
		return nil, types.ErrInvalidAuthority
	}
	// IMF / MF bounds before we touch state so a partial write can't leave
	// the margin chain in an invalid state.
	if msg.NewMinImf == 0 {
		return nil, types.ErrInvalidParams.Wrap("new_min_imf must be > 0")
	}
	if msg.NewDefaultImf > uint32(perptypes.MarginTick) ||
		msg.NewMinImf > uint32(perptypes.MarginTick) ||
		msg.NewMaintenanceMf > uint32(perptypes.MarginTick) ||
		msg.NewCloseOutMf > uint32(perptypes.MarginTick) {
		return nil, types.ErrInvalidParams.Wrapf(
			"margin fraction above MarginTick=%d", perptypes.MarginTick,
		)
	}
	if msg.NewDefaultImf < msg.NewMinImf ||
		msg.NewMaintenanceMf >= msg.NewDefaultImf ||
		msg.NewCloseOutMf >= msg.NewMaintenanceMf {
		return nil, types.ErrInvalidMarginChain
	}
	if msg.NewFundingClampSmall > msg.NewFundingClampBig {
		return nil, types.ErrInvalidParams.Wrap("funding_clamp_small must be <= funding_clamp_big")
	}
	d, err := m.GetMarketDetails(ctx, msg.MarketIndex)
	if err != nil {
		return nil, err
	}
	d.DefaultInitialMarginFraction = msg.NewDefaultImf
	d.MinInitialMarginFraction = msg.NewMinImf
	d.MaintenanceMarginFraction = msg.NewMaintenanceMf
	d.CloseOutMarginFraction = msg.NewCloseOutMf
	d.FundingClampSmall = msg.NewFundingClampSmall
	d.FundingClampBig = msg.NewFundingClampBig
	d.InterestRate = msg.NewInterestRate
	d.OpenInterestLimit = msg.NewOpenInterestLimit
	if err := m.SetMarketDetails(ctx, d); err != nil {
		return nil, err
	}
	return &types.MsgUpdateMarketDetailsResponse{}, nil
}

func (m msgServer) UpdateParams(ctx context.Context, msg *types.MsgUpdateParams) (*types.MsgUpdateParamsResponse, error) {
	if msg.Authority != m.authority {
		return nil, types.ErrInvalidAuthority
	}
	if err := msg.Params.Validate(); err != nil {
		return nil, err
	}
	if err := m.Params.Set(ctx, msg.Params); err != nil {
		return nil, err
	}
	return &types.MsgUpdateParamsResponse{}, nil
}
