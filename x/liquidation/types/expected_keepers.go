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
	GetPosition(ctx context.Context, accIdx uint64, marketIdx uint32) (accounttypes.AccountPosition, error)
	AddCollateral(ctx context.Context, idx uint64, delta math.Int) error
	IterateAccounts(ctx context.Context, cb func(accounttypes.Account) bool) error
	// IterateAccountPositions walks every persisted position row of
	// accountIdx; cb returns true to stop.
	IterateAccountPositions(ctx context.Context, accountIdx uint64, cb func(accounttypes.AccountPosition) bool) error
	IsAuthorized(ctx context.Context, signer string, idx uint64) (bool, error)
}

type MarketKeeper interface {
	GetMarket(ctx context.Context, idx uint32) (markettypes.Market, error)
	GetMarketDetails(ctx context.Context, idx uint32) (markettypes.MarketDetails, error)
	// GetMarkPriceAndDetails returns the gated mark and details in
	// one round-trip. Used by ADL ranking so uPnL ordering does not
	// need a full risk snapshot per position.
	GetMarkPriceAndDetails(ctx context.Context, marketIdx uint32) (uint32, markettypes.MarketDetails, error)
}

type RiskKeeper interface {
	GetHealthStatus(ctx context.Context, accountIdx uint64) (uint32, error)
	GetIsolatedHealthStatus(ctx context.Context, accountIdx uint64, marketIdx uint32) (uint32, error)
	// SimulateRiskAfterTakeover previews the cross risk the account
	// would have if it inherited delta of marketIdx at entryPrice.
	// Used by the LLP/IF takeover to enforce post.TAV >= post.IMR.
	// Errors on isolated targets so a misconfigured LLP/IF surfaces.
	SimulateRiskAfterTakeover(
		ctx context.Context,
		accountIdx uint64,
		marketIdx uint32,
		delta math.Int,
		entryPrice uint32,
	) (risktypes.RiskParameters, error)
	// GetLiquidationRiskSnapshot returns the cohesive (pos, mark, md,
	// Risk, CrossRisk, ZP) bundle for ADL ranking / autoADL self-gate.
	// Snapshots are values — rebuild after any state mutation or
	// downstream code sees stale TAV/MMR.
	GetLiquidationRiskSnapshot(
		ctx context.Context,
		accountIdx uint64,
		marketIdx uint32,
	) (risktypes.LiquidationRiskSnapshot, error)
	// GetZeroPriceSnapshot is the lightweight (pos, ZP) bundle for
	// callers that would discard the full snapshot's other fields.
	GetZeroPriceSnapshot(
		ctx context.Context,
		accountIdx uint64,
		marketIdx uint32,
	) (risktypes.ZeroPriceSnapshot, error)
	// ComputeCrossRisk powers Deleverage's post-fill HEALTHY check.
	ComputeCrossRisk(ctx context.Context, accountIdx uint64) (risktypes.RiskParameters, error)
}

type MatchingKeeper interface {
	// CancelAllOpenOrdersForAccount cancels every resting order of
	// accountIdx (no authority check; liquidation is the caller).
	// Returns the number of orders cancelled.
	CancelAllOpenOrdersForAccount(ctx context.Context, accountIdx uint64) (uint32, error)
	// MatchLiquidationOrder synthesises a victim-owned
	// LIQUIDATION_ORDER + IOC + reduce_only at zeroPrice and consumes
	// opposite makers up to baseAmount. Improvements above zeroPrice
	// are taxed at liquidationFeeBps → liquidationFeeRecipient.
	// IOC residue is discarded. The synthetic taker is never persisted
	// and bypasses the user-CreateOrder gates. Matching short-circuits
	// the moment the victim leaves PARTIAL/FULL liquidation health.
	MatchLiquidationOrder(
		ctx context.Context,
		victim uint64,
		marketIdx uint32,
		zeroPrice uint32,
		baseAmount uint64,
		liquidationFeeBps uint32,
		liquidationFeeRecipient uint64,
	) (uint64, error)
}

type TradeKeeper interface {
	ApplyPerpsMatching(ctx context.Context, f tradekeeper.PerpFill) error
}
