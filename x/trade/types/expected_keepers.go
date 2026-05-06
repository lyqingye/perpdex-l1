package types

import (
	"context"

	"cosmossdk.io/math"

	accounttypes "github.com/perpdex/perpdex-l1/x/account/types"
	markettypes "github.com/perpdex/perpdex-l1/x/market/types"
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
}

type FundingKeeper interface {
	SettlePositionFunding(ctx context.Context, accountIndex uint64, marketIndex uint32) error
}

type RiskKeeper interface {
	IsValidRiskChange(ctx context.Context, accountIndex uint64) (bool, error)
	// SnapshotPreRisk caches pre-state RiskParameters for an account so
	// IsValidRiskChange can require strict improvement on unhealthy
	// post-states.
	SnapshotPreRisk(ctx context.Context, accountIndex uint64) error
	// GetAvailableUsdcCollateral returns the amount of cross USDC
	// collateral free to fund an isolated margin allocation. Returns
	// zero when the account is not HEALTHY or `collateral_with_funding`
	// is negative. Used by ApplyPerpsMatching to refuse a fill whose
	// auto-allocated `margin_delta` would push the cross account out
	// of HEALTHY.
	GetAvailableUsdcCollateral(ctx context.Context, accountIndex uint64) (math.Int, error)
	// ComputePositionInitialMargin returns the IM requirement for a
	// hypothetical |posAbs| in `marketIdx` at the live mark price. The
	// trade keeper feeds this with old / new / OI-delta sizes when
	// computing the lighter `calculate_isolated_margin_change` deltas.
	ComputePositionInitialMargin(ctx context.Context, marketIdx uint32, posAbs math.Int) (math.Int, error)
	// ComputeUnrealizedPnLAt returns uPnL = position * mark -
	// entry_quote for caller-supplied position/entry values. Sister to
	// risk's `GetPositionUnrealizedPnL` that operates on a hypothetical
	// position (rather than the stored one) so the trade keeper can
	// reason about pre / post fill state cleanly.
	ComputeUnrealizedPnLAt(ctx context.Context, marketIdx uint32, position, entryQuote math.Int) (math.Int, error)
}
