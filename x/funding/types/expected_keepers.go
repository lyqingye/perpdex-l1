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
	// ComputeImpactPrice walks the requested side's resting depth using
	// the per-market impact notional derived from
	// MarketDetails.MinInitialMarginFraction (see x/orderbook
	// keeper.MarketImpactNotional) and returns the VWAP. The boolean is
	// false when the side has insufficient depth.
	ComputeImpactPrice(ctx context.Context, market uint32, isAsk bool) (uint32, bool, error)
}

type AccountKeeper interface {
	GetPosition(ctx context.Context, accIdx uint64, marketIdx uint32) (accounttypes.AccountPosition, error)
	// UpdatePosition is the canonical RMW entrypoint for position
	// state. Funding keeper uses it from SettlePositionFunding so the
	// snapshot bookkeeping + (future) event emission stay on the
	// account side.
	UpdatePosition(
		ctx context.Context,
		accIdx uint64,
		marketIdx uint32,
		mut func(*accounttypes.AccountPosition) error,
	) (accounttypes.AccountPosition, error)
}
