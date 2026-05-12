package keeper

import (
	"context"
	"strconv"

	"cosmossdk.io/math"

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

// resetRuntimeDetails zeroes every per-block mutable field on a
// MarketDetails record. Used at create time so a malicious / mistaken
// gov proposal cannot seed the market with poisoned runtime state
// (mark price, OI, premium sums, ...). ValidateBasic already rejects
// such payloads but this is the second layer of defence.
func resetRuntimeDetails(d *types.MarketDetails) {
	d.MarkPrice = 0
	d.IndexPrice = 0
	d.ImpactBidPrice = 0
	d.ImpactAskPrice = 0
	d.ImpactPrice = 0
	d.OpenInterest = 0
	d.AggregatePremiumSum = 0
	d.TotalOrderCount = 0
	d.TotalPremiumSamples = 0
	d.LastUpdatedTimestamp = 0
	d.FundingRatePrefixSum = math.ZeroInt()
	// QuoteMultiplier is derived by funding/risk on the fly; force 0
	// at create so the derivation runs from a clean slate.
	d.QuoteMultiplier = 0
}

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
	// QuoteAsset must exist and be enabled. Perps quote must be USDC
	// (the only margin-enabled asset in the genesis seed; see
	// x/asset). Spot base must also exist + be enabled.
	quote, err := m.assetKeeper.GetAsset(ctx, market.QuoteAssetId)
	if err != nil {
		return nil, err
	}
	if !quote.Enabled {
		return nil, types.ErrInvalidMarket.Wrapf(
			"quote_asset_id=%d is disabled", market.QuoteAssetId,
		)
	}
	switch market.MarketType {
	case perptypes.MarketTypePerps:
		if market.QuoteAssetId != perptypes.USDCAssetIndex {
			return nil, types.ErrInvalidMarket.Wrapf(
				"perps market quote_asset_id=%d must equal USDC=%d",
				market.QuoteAssetId, perptypes.USDCAssetIndex,
			)
		}
	case perptypes.MarketTypeSpot:
		base, err := m.assetKeeper.GetAsset(ctx, market.BaseAssetId)
		if err != nil {
			return nil, err
		}
		if !base.Enabled {
			return nil, types.ErrInvalidMarket.Wrapf(
				"base_asset_id=%d is disabled", market.BaseAssetId,
			)
		}
	}
	// Defense in depth (ValidateBasic already enforces ACTIVE +
	// canonical nonce/runtime values, but we re-stamp here so the
	// keeper-level invariants do not depend on Msg layer alone).
	market.CreatedAt = sdk.UnwrapSDKContext(ctx).BlockTime().UnixMilli()
	market.Status = perptypes.MarketStatusActive
	resetRuntimeDetails(&details)
	if err := m.createMarket(ctx, market); err != nil {
		return nil, err
	}
	if err := m.SetMarketDetails(ctx, details); err != nil {
		return nil, err
	}
	sdk.UnwrapSDKContext(ctx).EventManager().EmitEvent(sdk.NewEvent(
		types.EventTypeMarketCreated,
		sdk.NewAttribute(types.AttributeKeyMarketIndex, strconv.FormatUint(uint64(market.MarketIndex), 10)),
		sdk.NewAttribute(types.AttributeKeyMarketType, strconv.FormatUint(uint64(market.MarketType), 10)),
		sdk.NewAttribute(types.AttributeKeyBaseAssetID, strconv.FormatUint(uint64(market.BaseAssetId), 10)),
		sdk.NewAttribute(types.AttributeKeyQuoteAssetID, strconv.FormatUint(uint64(market.QuoteAssetId), 10)),
		sdk.NewAttribute(types.AttributeKeyCreatedAt, strconv.FormatInt(market.CreatedAt, 10)),
		sdk.NewAttribute(types.AttributeKeyExpiryTimestamp, strconv.FormatInt(market.ExpiryTimestamp, 10)),
	))
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
	// EXPIRED is a terminal state: once a market is EXPIRED, its
	// parameters are frozen. The only way to "re-list" is to create a
	// brand-new market at a fresh index.
	if market.Status == perptypes.MarketStatusExpired {
		return nil, types.ErrInvalidMarket.Wrap("cannot update expired market")
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
	oldMarket := market
	market.TakerFee = msg.NewTakerFee
	market.MakerFee = msg.NewMakerFee
	market.LiquidationFee = msg.NewLiquidationFee
	market.MinBaseAmount = msg.NewMinBaseAmount
	market.MinQuoteAmount = msg.NewMinQuoteAmount
	market.OrderQuoteLimit = msg.NewOrderQuoteLimit
	market.ExpiryTimestamp = msg.NewExpiryTimestamp
	market.Status = msg.NewStatus
	// updateMarket handles the field overlay and ExpiryIndex delta for
	// both the plain-update case and the "transition to EXPIRED" case
	// (in which the index entry is dropped because want-indexed
	// becomes false). For the EXPIRED transition we additionally need
	// to run applyMarketExit to close residual positions against the
	// insurance fund (H4). The two helpers compose without a redundant
	// Markets.Set so the manual delist path is now O(1) writes.
	if err := m.updateMarket(ctx, oldMarket, market); err != nil {
		return nil, err
	}
	if msg.NewStatus == perptypes.MarketStatusExpired {
		if err := m.applyMarketExit(ctx, market); err != nil {
			return nil, err
		}
	}
	sdk.UnwrapSDKContext(ctx).EventManager().EmitEvent(sdk.NewEvent(
		types.EventTypeMarketUpdated,
		sdk.NewAttribute(types.AttributeKeyMarketIndex, strconv.FormatUint(uint64(market.MarketIndex), 10)),
		sdk.NewAttribute(types.AttributeKeyStatus, strconv.FormatUint(uint64(msg.NewStatus), 10)),
		sdk.NewAttribute(types.AttributeKeyTakerFee, strconv.FormatUint(uint64(msg.NewTakerFee), 10)),
		sdk.NewAttribute(types.AttributeKeyMakerFee, strconv.FormatUint(uint64(msg.NewMakerFee), 10)),
		sdk.NewAttribute(types.AttributeKeyLiquidationFee, strconv.FormatUint(uint64(msg.NewLiquidationFee), 10)),
		sdk.NewAttribute(types.AttributeKeyMinBaseAmount, strconv.FormatUint(msg.NewMinBaseAmount, 10)),
		sdk.NewAttribute(types.AttributeKeyMinQuoteAmount, strconv.FormatUint(msg.NewMinQuoteAmount, 10)),
		sdk.NewAttribute(types.AttributeKeyOrderQuoteLimit, strconv.FormatInt(msg.NewOrderQuoteLimit, 10)),
		sdk.NewAttribute(types.AttributeKeyExpiryTimestamp, strconv.FormatInt(msg.NewExpiryTimestamp, 10)),
	))
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
	sdk.UnwrapSDKContext(ctx).EventManager().EmitEvent(sdk.NewEvent(
		types.EventTypeMarketDetailsUpdated,
		sdk.NewAttribute(types.AttributeKeyMarketIndex, strconv.FormatUint(uint64(msg.MarketIndex), 10)),
		sdk.NewAttribute(types.AttributeKeyDefaultImf, strconv.FormatUint(uint64(msg.NewDefaultImf), 10)),
		sdk.NewAttribute(types.AttributeKeyMinImf, strconv.FormatUint(uint64(msg.NewMinImf), 10)),
		sdk.NewAttribute(types.AttributeKeyMaintenanceMf, strconv.FormatUint(uint64(msg.NewMaintenanceMf), 10)),
		sdk.NewAttribute(types.AttributeKeyCloseOutMf, strconv.FormatUint(uint64(msg.NewCloseOutMf), 10)),
		sdk.NewAttribute(types.AttributeKeyFundingClampSmall, strconv.FormatUint(uint64(msg.NewFundingClampSmall), 10)),
		sdk.NewAttribute(types.AttributeKeyFundingClampBig, strconv.FormatUint(uint64(msg.NewFundingClampBig), 10)),
		sdk.NewAttribute(types.AttributeKeyInterestRate, strconv.FormatUint(uint64(msg.NewInterestRate), 10)),
		sdk.NewAttribute(types.AttributeKeyOpenInterestLimit, strconv.FormatUint(msg.NewOpenInterestLimit, 10)),
	))
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
	sdk.UnwrapSDKContext(ctx).EventManager().EmitEvent(sdk.NewEvent(
		types.EventTypeMarketParamsUpdated,
		sdk.NewAttribute(types.AttributeKeyMaxPerpsMarketIndex, strconv.FormatUint(uint64(msg.Params.MaxPerpsMarketIndex), 10)),
		sdk.NewAttribute(types.AttributeKeyMinSpotMarketIndex, strconv.FormatUint(uint64(msg.Params.MinSpotMarketIndex), 10)),
		sdk.NewAttribute(types.AttributeKeyMaxSpotMarketIndex, strconv.FormatUint(uint64(msg.Params.MaxSpotMarketIndex), 10)),
		sdk.NewAttribute(types.AttributeKeyMaxMarketsExpired, strconv.FormatUint(uint64(msg.Params.MaxMarketsExpiredPerBlock), 10)),
	))
	return &types.MsgUpdateParamsResponse{}, nil
}
