package types

import (
	"context"

	accounttypes "github.com/perpdex/perpdex-l1/x/account/types"
	markettypes "github.com/perpdex/perpdex-l1/x/market/types"
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
}

type TradeKeeper interface {
	ApplyPerpsMatching(ctx context.Context, f tradekeeper.Fill) error
	ApplySpotMatching(ctx context.Context, f tradekeeper.Fill, baseAssetID, quoteAssetID uint32) error
}
