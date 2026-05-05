package types

import (
	"context"

	"cosmossdk.io/math"

	accounttypes "github.com/perpdex/perpdex-l1/x/account/types"
	markettypes "github.com/perpdex/perpdex-l1/x/market/types"
	risktypes "github.com/perpdex/perpdex-l1/x/risk/types"
	tradekeeper "github.com/perpdex/perpdex-l1/x/trade/keeper"
)

type AccountKeeper interface {
	GetAccount(ctx context.Context, idx uint64) (accounttypes.Account, error)
	GetMasterAccountByOwner(ctx context.Context, owner string) (accounttypes.Account, error)
	GetPosition(ctx context.Context, accIdx uint64, marketIdx uint32) (accounttypes.AccountPosition, error)
	SetPosition(ctx context.Context, p accounttypes.AccountPosition) error
	AddCollateral(ctx context.Context, idx uint64, delta math.Int) error
	IterateAccounts(ctx context.Context, cb func(accounttypes.Account) bool) error
	IsAuthorized(ctx context.Context, signer string, idx uint64) (bool, error)
}

type MarketKeeper interface {
	GetMarket(ctx context.Context, idx uint32) (markettypes.Market, error)
	GetMarketDetails(ctx context.Context, idx uint32) (markettypes.MarketDetails, error)
}

type RiskKeeper interface {
	GetHealthStatus(ctx context.Context, accountIdx uint64) (uint32, error)
	GetIsolatedHealthStatus(ctx context.Context, accountIdx uint64, marketIdx uint32) (uint32, error)
	GetPositionZeroPrice(ctx context.Context, accountIdx uint64, marketIdx uint32) (uint32, error)
	GetPositionMarkValue(ctx context.Context, accountIdx uint64, marketIdx uint32) (math.Int, error)
	GetPositionUnrealizedPnL(ctx context.Context, accountIdx uint64, marketIdx uint32) (math.Int, error)
	// SimulateRiskAfterTakeover previews the cross risk parameters
	// the account would have if it inherited `delta` of `marketIdx`
	// settled at `entryPrice`. Used by the LLP / insurance fund
	// take-over routine to enforce "post.IM <= post.TAV".
	SimulateRiskAfterTakeover(
		ctx context.Context,
		accountIdx uint64,
		marketIdx uint32,
		delta math.Int,
		entryPrice uint32,
	) (risktypes.RiskParameters, error)
	// ComputeRiskInfo / ComputeIsolatedRisk are needed by the ADL
	// queue ranking and the LLP IMR check.
	ComputeRiskInfo(ctx context.Context, accountIdx uint64) (risktypes.RiskInfo, error)
	ComputeIsolatedRisk(ctx context.Context, accountIdx uint64, marketIdx uint32) (risktypes.RiskParameters, error)
}

type MatchingKeeper interface {
	// CancelAllOpenOrdersForAccount cancels every resting order
	// (regardless of market) owned by `accountIdx`. Authority/sender
	// checks are skipped because liquidation is the caller. Returns
	// the number of orders cancelled.
	CancelAllOpenOrdersForAccount(ctx context.Context, accountIdx uint64) (uint32, error)
}

type TradeKeeper interface {
	ApplyPerpsMatching(ctx context.Context, f tradekeeper.Fill) error
}
