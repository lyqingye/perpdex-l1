package types

import (
	"context"

	"cosmossdk.io/math"

	accounttypes "github.com/perpdex/perpdex-l1/x/account/types"
	markettypes "github.com/perpdex/perpdex-l1/x/market/types"
	tradekeeper "github.com/perpdex/perpdex-l1/x/trade/keeper"
)

type AccountKeeper interface {
	GetAccount(ctx context.Context, idx uint64) (accounttypes.Account, error)
	GetMasterAccountByOwner(ctx context.Context, owner string) (accounttypes.Account, error)
	GetPosition(ctx context.Context, accIdx uint64, marketIdx uint32) (accounttypes.AccountPosition, error)
	SetPosition(ctx context.Context, p accounttypes.AccountPosition) error
	AddCollateral(ctx context.Context, idx uint64, delta math.Int) error
	IterateAccounts(ctx context.Context, cb func(accounttypes.Account) bool) error
}

type MarketKeeper interface {
	GetMarket(ctx context.Context, idx uint32) (markettypes.Market, error)
	GetMarketDetails(ctx context.Context, idx uint32) (markettypes.MarketDetails, error)
}

type RiskKeeper interface {
	GetHealthStatus(ctx context.Context, accountIdx uint64) (uint32, error)
	GetPositionZeroPrice(ctx context.Context, accountIdx uint64, marketIdx uint32) (uint32, error)
}

type TradeKeeper interface {
	ApplyPerpsMatching(ctx context.Context, f tradekeeper.Fill) error
}
