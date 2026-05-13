package types

import (
	"context"

	"cosmossdk.io/math"

	accounttypes "github.com/perpdex/perpdex-l1/x/account/types"
	markettypes "github.com/perpdex/perpdex-l1/x/market/types"
	risktypes "github.com/perpdex/perpdex-l1/x/risk/types"
)

type AccountKeeper interface {
	GetAccount(ctx context.Context, idx uint64) (accounttypes.Account, error)
	GetPosition(ctx context.Context, accIdx uint64, marketIdx uint32) (accounttypes.AccountPosition, error)
	// UpdatePosition is the canonical RMW entrypoint for position
	// state. Trade keeper uses it for applyPositionChange /
	// applyPositionFinancials / applyIsolatedMargin so the post-state
	// invariants (bounds check, future events) live exclusively on
	// the account side.
	UpdatePosition(
		ctx context.Context,
		accIdx uint64,
		marketIdx uint32,
		mut func(*accounttypes.AccountPosition) error,
	) (accounttypes.AccountPosition, error)
	AddCollateral(ctx context.Context, idx uint64, delta math.Int) error
	GetAccountAsset(ctx context.Context, accIdx uint64, assetIdx uint32) (accounttypes.AccountAsset, error)
	// TransferAccountAssetBalance is the cohesive spot-balance move:
	// `drainLockedFirst=true` matches the maker / lock-on-place
	// semantics, `false` is the taker / fee path. Replaces direct
	// SetAccountAsset access from the trade keeper.
	TransferAccountAssetBalance(
		ctx context.Context,
		from, to uint64,
		assetIdx uint32,
		amount math.Int,
		drainLockedFirst bool,
	) error
}

type MarketKeeper interface {
	GetMarket(ctx context.Context, idx uint32) (markettypes.Market, error)
	GetMarketDetails(ctx context.Context, idx uint32) (markettypes.MarketDetails, error)
	UpdateOpenInterest(ctx context.Context, marketIdx uint32, delta int64) error
	// GetMarkPriceAndDetails returns the gated mark and MarketDetails for
	// `marketIdx`. Used by the isolated-margin auto-allocation path
	// (see perp.engine.allocateIsolatedMargin) so the engine can refuse
	// to seed margin against a stale or missing mark.
	GetMarkPriceAndDetails(ctx context.Context, marketIdx uint32) (uint32, markettypes.MarketDetails, error)
}

type FundingKeeper interface {
	SettlePositionFunding(ctx context.Context, accountIndex uint64, marketIndex uint32) error
}

type RiskKeeper interface {
	// SnapshotRisk computes the pre-state risk envelope for an account
	// and returns it by value. The caller threads the returned
	// snapshot into IsValidRiskChangeFrom after performing the state
	// mutation; the keeper does not persist pre-state across handlers.
	SnapshotRisk(ctx context.Context, accountIndex uint64) (risktypes.PreRiskSnapshot, error)
	// IsValidRiskChangeFrom enforces the post-state vs pre-state risk
	// invariants. `pre` MUST be the value returned by SnapshotRisk at
	// the start of the same handler.
	IsValidRiskChangeFrom(ctx context.Context, accountIndex uint64, pre risktypes.PreRiskSnapshot) (bool, error)
	// GetAvailableUsdcCollateral returns the amount of cross USDC
	// collateral free to fund an isolated margin allocation. Returns
	// zero when the account is not HEALTHY or `collateral_with_funding`
	// is negative. Used by ApplyPerpsMatching to refuse a fill whose
	// auto-allocated `margin_delta` would push the cross account out
	// of HEALTHY.
	GetAvailableUsdcCollateral(ctx context.Context, accountIndex uint64) (math.Int, error)
}
