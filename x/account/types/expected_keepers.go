package types

import (
	"context"

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

// The RiskKeeper expected interface lives in x/account/keeper:
// keeping it here would force x/account/types to import x/risk/types
// (for PreRiskSnapshot), which would create a cycle because
// x/risk/types already imports x/account/types for AccountPosition
// in the LiquidationRiskSnapshot. Per Go idiom, the interface lives
// next to the consumer (x/account/keeper) anyway.
