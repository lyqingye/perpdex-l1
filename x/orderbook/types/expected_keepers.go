package types

import (
	"context"

	"cosmossdk.io/math"

	markettypes "github.com/perpdex/perpdex-l1/x/market/types"
)

// MarketKeeper is the narrow read-side surface orderbook needs to
// validate orders and allocate per-side nonces. Mutations on
// MarketDetails (e.g. funding-rate accrual) live behind their own
// module boundary and are deliberately NOT exposed here.
type MarketKeeper interface {
	GetMarket(ctx context.Context, idx uint32) (markettypes.Market, error)
	GetMarketDetails(ctx context.Context, idx uint32) (markettypes.MarketDetails, error)
	AllocateNonce(ctx context.Context, marketIdx uint32, isAsk bool) (int64, error)
}

// SpotLocker is the narrow surface orderbook needs to enforce
// lock-on-place for spot resting orders. Implemented by x/account.
//
// Mirrors `increment_locked_balance_for_order` /
// `decrement_locked_balance_for_order`, which are called whenever a
// spot limit order rests on / leaves the orderbook.
type SpotLocker interface {
	IncreaseLockedBalance(ctx context.Context, accIdx uint64, assetIdx uint32, amount math.Int) error
	DecreaseLockedBalance(ctx context.Context, accIdx uint64, assetIdx uint32, amount math.Int) error
}
