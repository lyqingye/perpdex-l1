package types

import (
	"context"

	"cosmossdk.io/math"

	markettypes "github.com/perpdex/perpdex-l1/x/market/types"
)

type MarketKeeper interface {
	GetMarket(ctx context.Context, idx uint32) (markettypes.Market, error)
	GetMarketDetails(ctx context.Context, idx uint32) (markettypes.MarketDetails, error)
	AllocateNonce(ctx context.Context, marketIdx uint32, isAsk bool) (int64, error)
	SetMarketDetails(ctx context.Context, d markettypes.MarketDetails) error
}

// SpotLocker is the narrow surface orderbook needs to enforce
// lock-on-place for spot resting orders. Implemented by x/account.
//
// Lighter parity: `increment_locked_balance_for_order` /
// `decrement_locked_balance_for_order` are called whenever a spot
// limit order rests on / leaves the orderbook.
type SpotLocker interface {
	IncreaseLockedBalance(ctx context.Context, accIdx uint64, assetIdx uint32, amount math.Int) error
	DecreaseLockedBalance(ctx context.Context, accIdx uint64, assetIdx uint32, amount math.Int) error
}
