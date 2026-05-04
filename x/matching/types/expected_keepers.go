package types

import (
	"context"

	accounttypes "github.com/perpdex/perpdex-l1/x/account/types"
	markettypes "github.com/perpdex/perpdex-l1/x/market/types"
	oracletypes "github.com/perpdex/perpdex-l1/x/oracle/types"
	orderbooktypes "github.com/perpdex/perpdex-l1/x/orderbook/types"
	tradekeeper "github.com/perpdex/perpdex-l1/x/trade/keeper"
)

type AccountKeeper interface {
	GetAccount(ctx context.Context, idx uint64) (accounttypes.Account, error)
	GetPosition(ctx context.Context, accIdx uint64, marketIdx uint32) (accounttypes.AccountPosition, error)
	IsAuthorized(ctx context.Context, signer string, idx uint64) (bool, error)
}

type MarketKeeper interface {
	GetMarket(ctx context.Context, idx uint32) (markettypes.Market, error)
	GetMarketDetails(ctx context.Context, idx uint32) (markettypes.MarketDetails, error)
	AllocateNonce(ctx context.Context, marketIdx uint32, isAsk bool) (int64, error)
}

type OrderbookKeeper interface {
	GetOrder(ctx context.Context, idx uint64) (orderbooktypes.Order, error)
	SetOrder(ctx context.Context, o orderbooktypes.Order) error
	GetOrderByClientID(ctx context.Context, market uint32, account uint64, clientID uint64) (orderbooktypes.Order, error)
	AllocateOrderIndex(ctx context.Context) (uint64, error)
	InsertOrderbookEntry(ctx context.Context, market uint32, isAsk bool, o orderbooktypes.OrderBookEntry) error
	RemoveOrderbookEntry(ctx context.Context, market uint32, isAsk bool, orderIndex uint64) error
	PartialFill(ctx context.Context, market uint32, isAsk bool, orderIndex uint64, filledBase uint64) error
	PeekBestOpposite(ctx context.Context, market uint32, isAsk bool) (orderbooktypes.OrderBookEntry, bool, error)
	WouldCross(ctx context.Context, market uint32, isAsk bool, price uint32) (bool, error)
	IndexClientOrder(ctx context.Context, o orderbooktypes.Order) error
	UnindexClientOrder(ctx context.Context, o orderbooktypes.Order) error
	// UnindexClientOrderIfMatches removes the (market, account, client_id)
	// -> order_index mapping only when it still points at `o.OrderIndex`.
	// Prevents a cancel from wiping the index of a freshly-created new
	// order that happens to share the same client_order_index.
	UnindexClientOrderIfMatches(ctx context.Context, o orderbooktypes.Order) error
	// HasOpenClientOrder reports whether (market, account, clientID)
	// currently resolves to an open/pending order in the book.
	HasOpenClientOrder(ctx context.Context, market uint32, account uint64, clientID uint64) (bool, uint64, error)
	AddTrigger(ctx context.Context, market uint32, triggerPrice uint32, orderIndex uint64) error
	RemoveTrigger(ctx context.Context, market uint32, triggerPrice uint32, orderIndex uint64) error
	IterateTriggers(ctx context.Context, cb func(market uint32, triggerPrice uint32, orderIndex uint64) bool) error
	// IterateUserOrders walks every (market, account, clientID) ->
	// order_index mapping owned by `account`. `cb` returns true to stop.
	IterateUserOrders(ctx context.Context, account uint64, cb func(orderbooktypes.Order) bool) error
	// IndexAccountOpenOrder marks an order as resting/non-terminal so
	// cancel-all can find it regardless of client_order_index.
	IndexAccountOpenOrder(ctx context.Context, o orderbooktypes.Order) error
	// UnindexAccountOpenOrder removes the resting marker for `o`.
	UnindexAccountOpenOrder(ctx context.Context, o orderbooktypes.Order) error
	// IterateAccountOpenOrders yields every resting order owned by
	// `account`, optionally filtered by market (0 = all markets).
	IterateAccountOpenOrders(ctx context.Context, account uint64, marketFilter uint32, cb func(orderbooktypes.Order) bool) error
}

type TradeKeeper interface {
	ApplyPerpsMatching(ctx context.Context, f tradekeeper.Fill) error
	ApplySpotMatching(ctx context.Context, f tradekeeper.Fill, baseAssetID, quoteAssetID uint32) error
}

// OracleKeeper provides the mark price used by the matching EndBlocker to
// resolve trigger orders.
type OracleKeeper interface {
	GetPrice(ctx context.Context, marketIdx uint32) (oracletypes.OraclePrice, error)
}
