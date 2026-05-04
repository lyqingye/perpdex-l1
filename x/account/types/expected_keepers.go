package types

import (
	"context"

	"cosmossdk.io/math"

	sdk "github.com/cosmos/cosmos-sdk/types"

	assettypes "github.com/perpdex/perpdex-l1/x/asset/types"
	markettypes "github.com/perpdex/perpdex-l1/x/market/types"
)

// AssetKeeper is the subset of the asset keeper interface required by x/account.
type AssetKeeper interface {
	GetAsset(ctx context.Context, index uint32) (assettypes.Asset, error)
	GetAssetByDenom(ctx context.Context, denom string) (assettypes.Asset, error)
}

// BankKeeper is the bank keeper subset required by x/account.
type BankKeeper interface {
	SendCoinsFromAccountToModule(ctx context.Context, sender sdk.AccAddress, recipientModule string, amt sdk.Coins) error
	SendCoinsFromModuleToAccount(ctx context.Context, senderModule string, recipient sdk.AccAddress, amt sdk.Coins) error
	GetBalance(ctx context.Context, addr sdk.AccAddress, denom string) sdk.Coin
}

// FundingKeeper allows account-level operations to settle pending funding for
// the touched positions before applying state changes.
type FundingKeeper interface {
	SettlePositionFunding(ctx context.Context, accountIndex uint64, marketIndex uint32) error
}

// MarketKeeper lets account validate `market` related fields such as
// `UpdateLeverage`'s margin chain (default/min IMF) without importing the
// full market keeper.
type MarketKeeper interface {
	GetMarketDetails(ctx context.Context, index uint32) (markettypes.MarketDetails, error)
	GetMarket(ctx context.Context, index uint32) (markettypes.Market, error)
}

// RiskKeeper is consulted to ensure each state change leaves the account
// healthy or strictly improving.
type RiskKeeper interface {
	IsValidRiskChange(ctx context.Context, accountIndex uint64) (bool, error)
	GetAvailableCollateral(ctx context.Context, accountIndex uint64) (math.Int, error)
	// GetTotalAccountValue returns TAV (collateral + signed unrealized PnL)
	// across every market. Used for share NAV calculations.
	GetTotalAccountValue(ctx context.Context, accountIndex uint64) (math.Int, error)
	// GetHealthStatus mirrors x/risk health classification used by the
	// freeze invariants (freeze requires HEALTHY).
	GetHealthStatus(ctx context.Context, accountIndex uint64) (uint32, error)
	// SnapshotPreRisk caches the current risk parameters for an account
	// so a later IsValidRiskChange call can compare against them rather
	// than only accepting strictly-healthy post-states.
	SnapshotPreRisk(ctx context.Context, accountIndex uint64) error
}
