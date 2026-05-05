package types

import (
	"context"

	accounttypes "github.com/perpdex/perpdex-l1/x/account/types"
	markettypes "github.com/perpdex/perpdex-l1/x/market/types"
	oracletypes "github.com/perpdex/perpdex-l1/x/oracle/types"
)

type MarketKeeper interface {
	GetMarket(ctx context.Context, idx uint32) (markettypes.Market, error)
	GetMarketDetails(ctx context.Context, idx uint32) (markettypes.MarketDetails, error)
	SetMarketDetails(ctx context.Context, d markettypes.MarketDetails) error
	IterateMarkets(ctx context.Context, cb func(markettypes.Market) bool) error
}

type OracleKeeper interface {
	GetPrice(ctx context.Context, marketIdx uint32) (oracletypes.OraclePrice, error)
}

type OrderbookKeeper interface {
	BestBidAsk(ctx context.Context, market uint32) (uint32, uint32, error)
	ComputeImpactPrice(ctx context.Context, market uint32, isAsk bool, usdcAmount uint64) (uint32, bool, error)
	// ImpactUsdcAmount returns the governance-configured impact notional
	// (`Params.ImpactUsdcAmount` in x/orderbook). Funding samples the
	// VWAP across this much resting quote depth on each side of the book
	// to derive the per-minute premium.
	ImpactUsdcAmount(ctx context.Context) (uint64, error)
}

type AccountKeeper interface {
	GetPosition(ctx context.Context, accIdx uint64, marketIdx uint32) (accounttypes.AccountPosition, error)
	SetPosition(ctx context.Context, p accounttypes.AccountPosition) error
}
