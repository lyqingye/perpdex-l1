package types

import (
	"context"

	"cosmossdk.io/math"

	accounttypes "github.com/perpdex/perpdex-l1/x/account/types"
	markettypes "github.com/perpdex/perpdex-l1/x/market/types"
)

type AccountKeeper interface {
	GetAccount(ctx context.Context, idx uint64) (accounttypes.Account, error)
	SetAccount(ctx context.Context, a accounttypes.Account) error
	GetPosition(ctx context.Context, accIdx uint64, marketIdx uint32) (accounttypes.AccountPosition, error)
	SetPosition(ctx context.Context, p accounttypes.AccountPosition) error
	AddCollateral(ctx context.Context, idx uint64, delta math.Int) error
	GetAccountAsset(ctx context.Context, accIdx uint64, assetIdx uint32) (accounttypes.AccountAsset, error)
	SetAccountAsset(ctx context.Context, aa accounttypes.AccountAsset) error
}

type MarketKeeper interface {
	GetMarket(ctx context.Context, idx uint32) (markettypes.Market, error)
	GetMarketDetails(ctx context.Context, idx uint32) (markettypes.MarketDetails, error)
	UpdateOpenInterest(ctx context.Context, marketIdx uint32, delta int64) error
}

type FundingKeeper interface {
	SettlePositionFunding(ctx context.Context, accountIndex uint64, marketIndex uint32) error
}

type RiskKeeper interface {
	IsValidRiskChange(ctx context.Context, accountIndex uint64) (bool, error)
}
