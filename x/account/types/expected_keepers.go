package types

import (
	"context"

	"cosmossdk.io/math"

	sdk "github.com/cosmos/cosmos-sdk/types"

	assettypes "github.com/perpdex/perpdex-l1/x/asset/types"
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

// RiskKeeper is consulted to ensure each state change leaves the account
// healthy or strictly improving.
type RiskKeeper interface {
	IsValidRiskChange(ctx context.Context, accountIndex uint64) (bool, error)
	GetAvailableCollateral(ctx context.Context, accountIndex uint64) (math.Int, error)
}
