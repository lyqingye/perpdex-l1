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
	// ApplyFill is the cohesive fill-application entry-point (issue
	// #91). It owns the full open / mutate / close / flip dispatch,
	// persistence and lifecycle event emission for one side of one
	// fill; the trade engine consumes the returned FillApplyResult
	// for downstream fee / isolated-reconciliation / OI / risk-check
	// pipelines without issuing any position-keeper RMW closure of
	// its own.
	ApplyFill(
		ctx context.Context,
		accIdx uint64,
		marketIdx uint32,
		price uint32,
		baseAmount uint64,
		sign int64,
		fundingRatePrefixSum math.Int,
	) (accounttypes.FillApplyResult, error)
	// AdjustAllocatedMargin folds `delta` (signed) into the isolated
	// allocated_margin pool. Used by the engine's three-step isolated
	// reconciliation (PnL/fee credit, improvement-fee debit,
	// position_requirement rebalance). Asserts the position is open.
	AdjustAllocatedMargin(
		ctx context.Context,
		accIdx uint64,
		marketIdx uint32,
		delta math.Int,
	) (accounttypes.AccountPosition, error)
	AddCollateral(ctx context.Context, idx uint64, delta math.Int) error
	GetAccountAsset(ctx context.Context, accIdx uint64, assetIdx uint32) (accounttypes.AccountAsset, error)
	// TransferAccountAssetBalance moves a spot balance.
	// drainLockedFirst=true matches maker lock-on-place; false is
	// the taker / fee path.
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
	// GetMarkPriceAndDetails returns the gated mark and details so
	// the isolated-margin auto-allocation path refuses to seed margin
	// against a stale or missing mark.
	GetMarkPriceAndDetails(ctx context.Context, marketIdx uint32) (uint32, markettypes.MarketDetails, error)
}

type FundingKeeper interface {
	SettlePositionFunding(ctx context.Context, accountIndex uint64, marketIndex uint32) error
}

type RiskKeeper interface {
	// SnapshotRisk returns the pre-state risk envelope by value.
	// Pre-state is not persisted; the caller threads it through to
	// IsValidRiskChangeFrom in the same handler.
	SnapshotRisk(ctx context.Context, accountIndex uint64) (risktypes.PreRiskSnapshot, error)
	// IsValidRiskChangeFrom enforces the post-vs-pre risk invariants.
	// pre MUST come from SnapshotRisk in the same handler.
	IsValidRiskChangeFrom(ctx context.Context, accountIndex uint64, pre risktypes.PreRiskSnapshot) (bool, error)
	// GetAvailableUsdcCollateral returns the cross USDC free to fund
	// an isolated margin allocation. Returns zero when the account is
	// not HEALTHY or collateral_with_funding is negative.
	GetAvailableUsdcCollateral(ctx context.Context, accountIndex uint64) (math.Int, error)
}
