package types

import (
	"context"

	"cosmossdk.io/math"

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
	// the per-market impact notional (see
	// x/orderbook keeper.MarketImpactNotional) and returns the VWAP.
	// Returns 0 when the side has insufficient depth.
	ComputeImpactPrice(ctx context.Context, market uint32, isAsk bool) (uint32, error)
}

type AccountKeeper interface {
	GetPosition(ctx context.Context, accIdx uint64, marketIdx uint32) (accounttypes.AccountPosition, error)
	// ApplyFundingPayment is the cohesive funding-settlement RMW
	// (issue #91). Folds the per-position payment into EntryQuote
	// and snapshots the new prefix sum, in one keeper call. No-op on
	// empty rows (closed / never-opened positions have no funding
	// obligation; the next ApplyFill re-seeds the snapshot from the
	// market's current value).
	ApplyFundingPayment(
		ctx context.Context,
		accIdx uint64,
		marketIdx uint32,
		newPrefixSum math.Int,
	) (accounttypes.AccountPosition, error)
}
