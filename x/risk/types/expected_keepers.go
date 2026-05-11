package types

import (
	"context"

	"cosmossdk.io/math"

	accounttypes "github.com/perpdex/perpdex-l1/x/account/types"
	markettypes "github.com/perpdex/perpdex-l1/x/market/types"
	oracletypes "github.com/perpdex/perpdex-l1/x/oracle/types"
)

type AccountKeeper interface {
	GetAccount(ctx context.Context, idx uint64) (accounttypes.Account, error)
	GetPosition(ctx context.Context, accIdx uint64, marketIdx uint32) (accounttypes.AccountPosition, error)
	GetAccountAsset(ctx context.Context, accIdx uint64, assetIdx uint32) (accounttypes.AccountAsset, error)
	IterateAccounts(ctx context.Context, cb func(accounttypes.Account) bool) error
	// IterateAccountPositions walks every persisted position row owned by
	// `accountIdx`. Callback returns true to stop. Replaces the old
	// MaxPerpsMarketIndex full-scan loops in risk / liquidation.
	IterateAccountPositions(ctx context.Context, accountIdx uint64, cb func(accounttypes.AccountPosition) bool) error
}

type MarketKeeper interface {
	GetMarket(ctx context.Context, idx uint32) (markettypes.Market, error)
	GetMarketDetails(ctx context.Context, idx uint32) (markettypes.MarketDetails, error)
}

type OracleKeeper interface {
	GetPrice(ctx context.Context, marketIdx uint32) (oracletypes.OraclePrice, error)
}

// Helpers used by tests.
type RiskCalc interface {
	ComputeCrossRisk(ctx context.Context, accountIdx uint64) (RiskParameters, error)
	GetAvailableCollateral(ctx context.Context, accountIdx uint64) (math.Int, error)
	SnapshotRisk(ctx context.Context, accountIdx uint64) (PreRiskSnapshot, error)
	IsValidRiskChangeFrom(ctx context.Context, accountIdx uint64, pre PreRiskSnapshot) (bool, error)
}
