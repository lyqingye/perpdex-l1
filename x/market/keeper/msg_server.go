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
	if err := msg.ValidateBasic(); err != nil {
		return nil, err
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
	market, err := m.GetMarket(ctx, msg.MarketIndex)
	if err != nil {
		return nil, err
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

func (m msgServer) UpdateMarketDetails(ctx context.Context, msg *types.MsgUpdateMarketDetails) (*types.MsgUpdateMarketDetailsResponse, error) {
	if msg.Authority != m.authority {
		return nil, types.ErrInvalidAuthority
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
	if d.DefaultInitialMarginFraction < d.MinInitialMarginFraction ||
		d.MaintenanceMarginFraction >= d.DefaultInitialMarginFraction ||
		d.CloseOutMarginFraction >= d.MaintenanceMarginFraction {
		return nil, types.ErrInvalidMarginChain
	}
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
