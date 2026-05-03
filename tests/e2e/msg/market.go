package msg

import (
	"cosmossdk.io/math"

	sdk "github.com/cosmos/cosmos-sdk/types"

	perp "github.com/perpdex/perpdex-l1/app"
	perptypes "github.com/perpdex/perpdex-l1/types"
	marketkeeper "github.com/perpdex/perpdex-l1/x/market/keeper"
	markettypes "github.com/perpdex/perpdex-l1/x/market/types"
)

// MarketOpts groups the MarketIndex/asset pair plus the most-frequently
// tweaked numeric knobs. All other fields use sensible defaults derived
// from the chain-wide perptypes constants.
type MarketOpts struct {
	MarketIndex      uint32
	MarketType       uint32 // perptypes.MarketTypePerps / Spot
	BaseAssetID      uint32
	QuoteAssetID     uint32
	TakerFee         uint32 // fee_tick units (1_000_000 = 100%)
	MakerFee         uint32
	LiquidationFee   uint32
	OpenInterestCap  uint64
	DefaultIMF       uint32 // margin_tick units (10_000 = 100%)
	MinIMF           uint32
	MaintenanceMF    uint32
	CloseOutMF       uint32
	FundingClampSm   uint32
	FundingClampBig  uint32
	MinBaseAmount    uint64
	MinQuoteAmount   uint64
	OrderQuoteLimit  int64
	ExpiryTimestamp  int64
}

// DefaultPerpMarketOpts returns a MarketOpts good for most happy-path
// trading flow tests: 10bps fees, 5% IMF, 2.5% MMF, 1.25% close-out,
// USDC quote, and a generous OI cap.
func DefaultPerpMarketOpts(marketIndex uint32, baseAssetID uint32) MarketOpts {
	return MarketOpts{
		MarketIndex:     marketIndex,
		MarketType:      perptypes.MarketTypePerps,
		BaseAssetID:     baseAssetID,
		QuoteAssetID:    perptypes.USDCAssetIndex,
		TakerFee:        1_000,                                       // 0.10%
		MakerFee:        500,                                         // 0.05%
		LiquidationFee:  2_000,                                       // 0.20%
		OpenInterestCap: 1_000_000_000_000,                           // very large cap
		DefaultIMF:      500,                                         // 5.0%
		MinIMF:          500,                                         // 5.0%
		MaintenanceMF:   250,                                         // 2.5%
		CloseOutMF:      125,                                         // 1.25%
		FundingClampSm:  uint32(perptypes.FundingSmallClamp),
		FundingClampBig: uint32(perptypes.FundingBigClamp),
		MinBaseAmount:   1,
		MinQuoteAmount:  1,
		OrderQuoteLimit: int64(perptypes.MaxOrderQuoteAmount),
	}
}

// CreateMarket dispatches MsgCreateMarket as governance.
func CreateMarket(
	app *perp.PerpDEXApp,
	ctx sdk.Context,
	govAddr sdk.AccAddress,
	opts MarketOpts,
) (*markettypes.MsgCreateMarketResponse, error) {
	srv := marketkeeper.NewMsgServerImpl(app.MarketKeeper)
	market := markettypes.Market{
		MarketIndex:              opts.MarketIndex,
		Status:                   perptypes.MarketStatusActive,
		MarketType:               opts.MarketType,
		BaseAssetId:              opts.BaseAssetID,
		QuoteAssetId:             opts.QuoteAssetID,
		TakerFee:                 opts.TakerFee,
		MakerFee:                 opts.MakerFee,
		LiquidationFee:           opts.LiquidationFee,
		SizeExtensionMultiplier:  1,
		QuoteExtensionMultiplier: 1,
		MinBaseAmount:            opts.MinBaseAmount,
		MinQuoteAmount:           opts.MinQuoteAmount,
		OrderQuoteLimit:          opts.OrderQuoteLimit,
		ExpiryTimestamp:          opts.ExpiryTimestamp,
	}
	details := markettypes.MarketDetails{
		MarketIndex:                  opts.MarketIndex,
		DefaultInitialMarginFraction: opts.DefaultIMF,
		MinInitialMarginFraction:     opts.MinIMF,
		MaintenanceMarginFraction:    opts.MaintenanceMF,
		CloseOutMarginFraction:       opts.CloseOutMF,
		QuoteMultiplier:              1,
		InterestRate:                 0,
		FundingRatePrefixSum:         math.ZeroInt(),
		OpenInterestLimit:            opts.OpenInterestCap,
		AskNonce:                     perptypes.FirstAskNonce,
		BidNonce:                     perptypes.FirstBidNonce,
		FundingClampSmall:            opts.FundingClampSm,
		FundingClampBig:              opts.FundingClampBig,
	}
	return srv.CreateMarket(ctx, &markettypes.MsgCreateMarket{
		Authority:     govAddr.String(),
		Market:        market,
		MarketDetails: details,
	})
}

// UpdateMarket flips the status / fees / expiry of an existing market via
// gov authority. Pass `keep` for any field that should retain its value
// (the helper reads the current market so callers don't have to).
func UpdateMarket(
	app *perp.PerpDEXApp,
	ctx sdk.Context,
	govAddr sdk.AccAddress,
	marketIndex uint32,
	newStatus uint32,
	expiryTimestamp int64,
) (*markettypes.MsgUpdateMarketResponse, error) {
	srv := marketkeeper.NewMsgServerImpl(app.MarketKeeper)
	market, err := app.MarketKeeper.GetMarket(ctx, marketIndex)
	if err != nil {
		return nil, err
	}
	return srv.UpdateMarket(ctx, &markettypes.MsgUpdateMarket{
		Authority:          govAddr.String(),
		MarketIndex:        marketIndex,
		NewStatus:          newStatus,
		NewTakerFee:        market.TakerFee,
		NewMakerFee:        market.MakerFee,
		NewMinBaseAmount:   market.MinBaseAmount,
		NewMinQuoteAmount:  market.MinQuoteAmount,
		NewOrderQuoteLimit: market.OrderQuoteLimit,
		NewExpiryTimestamp: expiryTimestamp,
	})
}
