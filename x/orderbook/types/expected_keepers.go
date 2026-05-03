package types

import (
	"context"

	markettypes "github.com/perpdex/perpdex-l1/x/market/types"
)

type MarketKeeper interface {
	GetMarket(ctx context.Context, idx uint32) (markettypes.Market, error)
	GetMarketDetails(ctx context.Context, idx uint32) (markettypes.MarketDetails, error)
	AllocateNonce(ctx context.Context, marketIdx uint32, isAsk bool) (int64, error)
	SetMarketDetails(ctx context.Context, d markettypes.MarketDetails) error
}
