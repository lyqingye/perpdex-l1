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

// Every msg_server handler in this module starts with msg.ValidateBasic()
// as defense-in-depth: SDK v0.53 ante (`ValidateBasicDecorator`) only
// runs `tx.ValidateBasic()` and never iterates Msgs to call
// `Msg.ValidateBasic()`. Keeper-level test callers, governance
// proposals, and hypothetical cross-module callers can therefore
// bypass the stateless invariants unless every handler re-runs them
// at the entry point.
func (m msgServer) CreateMarket(ctx context.Context, msg *types.MsgCreateMarket) (*types.MsgCreateMarketResponse, error) {
	if err := msg.ValidateBasic(); err != nil {
		return nil, err
	}
	if msg.Authority != m.authority {
		return nil, types.ErrInvalidAuthority
	}
	market := msg.Market
	details := msg.MarketDetails
	exists, err := m.Markets.Has(ctx, market.MarketIndex)
	if err != nil {
		return nil, err
	}
	if exists {
		return nil, types.ErrMarketExists
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
	if err := msg.ValidateBasic(); err != nil {
		return nil, err
	}
	if msg.Authority != m.authority {
		return nil, types.ErrInvalidAuthority
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

func (m msgServer) UpdateMarketDetails(ctx context.Context, msg *types.MsgUpdateMarketDetails) (*types.MsgUpdateMarketDetailsResponse, error) {
	if err := msg.ValidateBasic(); err != nil {
		return nil, err
	}
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
	if err := m.SetMarketDetails(ctx, d); err != nil {
		return nil, err
	}
	return &types.MsgUpdateMarketDetailsResponse{}, nil
}

func (m msgServer) UpdateParams(ctx context.Context, msg *types.MsgUpdateParams) (*types.MsgUpdateParamsResponse, error) {
	if err := msg.ValidateBasic(); err != nil {
		return nil, err
	}
	if msg.Authority != m.authority {
		return nil, types.ErrInvalidAuthority
	}
	if err := m.Params.Set(ctx, msg.Params); err != nil {
		return nil, err
	}
	return &types.MsgUpdateParamsResponse{}, nil
}
