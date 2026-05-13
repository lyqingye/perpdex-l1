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
	// UpdatePosition is the canonical RMW entrypoint for position
	// state. The liquidation keeper currently does not write
	// positions directly, but the interface keeps parity with x/trade
	// and x/funding so that any future liquidation-side mutator
	// continues to flow through the cohesive choke point.
	UpdatePosition(
		ctx context.Context,
		accIdx uint64,
		marketIdx uint32,
		mut func(*accounttypes.AccountPosition) error,
	) (accounttypes.AccountPosition, error)
	AddCollateral(ctx context.Context, idx uint64, delta math.Int) error
	IterateAccounts(ctx context.Context, cb func(accounttypes.Account) bool) error
	// IterateAccountPositions walks every persisted position row owned by
	// `accountIdx`. Callback returns true to stop.
	IterateAccountPositions(ctx context.Context, accountIdx uint64, cb func(accounttypes.AccountPosition) bool) error
	IsAuthorized(ctx context.Context, signer string, idx uint64) (bool, error)
}

type MarketKeeper interface {
	GetMarket(ctx context.Context, idx uint32) (markettypes.Market, error)
	GetMarketDetails(ctx context.Context, idx uint32) (markettypes.MarketDetails, error)
	// GetMarkPriceAndDetails returns the gated mark and MarketDetails row
	// for `marketIdx`. Used by ADL ranking (rankVictimPositionsByUPnL)
	// which only needs the mark for ascending-uPnL ordering and would
	// otherwise have to build a full risk snapshot per ranked position.
	GetMarkPriceAndDetails(ctx context.Context, marketIdx uint32) (uint32, markettypes.MarketDetails, error)
}

type RiskKeeper interface {
	GetHealthStatus(ctx context.Context, accountIdx uint64) (uint32, error)
	GetIsolatedHealthStatus(ctx context.Context, accountIdx uint64, marketIdx uint32) (uint32, error)
	// SimulateRiskAfterTakeover previews the cross risk parameters
	// the account would have if it inherited `delta` of `marketIdx`
	// settled at `entryPrice`. Used by the LLP / insurance fund
	// take-over routine to enforce "post.IM <= post.TAV". Refuses
	// isolated targets with an error so an LLP/IF position
	// misconfigured as isolated is surfaced rather than silently
	// mis-simulated.
	SimulateRiskAfterTakeover(
		ctx context.Context,
		accountIdx uint64,
		marketIdx uint32,
		delta math.Int,
		entryPrice uint32,
	) (risktypes.RiskParameters, error)
	// GetLiquidationRiskSnapshot returns the cohesive (pos, mark,
	// md, Risk, CrossRisk, ZeroPrice) bundle for one (account,
	// market) pair. Scoped to ADL ranking and autoADL — those
	// callers consume `snap.Risk` / `snap.CrossRisk` for leverage
	// scoring and the FULL/BANKRUPTCY self-gate. Liquidate /
	// Deleverage use GetZeroPriceSnapshot instead because they only
	// need the position and zero price.
	//
	// Snapshots are values: they represent the state at the moment
	// of the call and MUST be re-built after any state mutation.
	// Threading a snapshot across a fill / settlement boundary will
	// feed stale TAV / MMR into downstream computations.
	GetLiquidationRiskSnapshot(
		ctx context.Context,
		accountIdx uint64,
		marketIdx uint32,
	) (risktypes.LiquidationRiskSnapshot, error)
	// GetZeroPriceSnapshot is the lightweight (pos, ZeroPrice)
	// counterpart used by the Liquidate and Deleverage Msg handlers
	// (and the gRPC zero-price query) where the Risk / CrossRisk
	// envelopes from the full snapshot would just be discarded.
	GetZeroPriceSnapshot(
		ctx context.Context,
		accountIdx uint64,
		marketIdx uint32,
	) (risktypes.ZeroPriceSnapshot, error)
}

// FundingKeeper provides funding-settlement primitives. Used by the
// pre-trade collateral assert in Deleverage so the bankrupt /
// deleverager balances the assert reads are funding-aware: any pending
// funding for the targeted market position is realised before we
// compare available collateral against the predicted realised PnL.
// Settling funding is idempotent and rolls accrued obligations into
// `EntryQuote`, mirroring what `Engine.Apply`'s step-1 will do
// immediately afterwards.
type FundingKeeper interface {
	SettlePositionFunding(ctx context.Context, accountIndex uint64, marketIndex uint32) error
}

type MatchingKeeper interface {
	// CancelAllOpenOrdersForAccount cancels every resting order
	// (regardless of market) owned by `accountIdx`. Authority/sender
	// checks are skipped because liquidation is the caller. Returns
	// the number of orders cancelled.
	CancelAllOpenOrdersForAccount(ctx context.Context, accountIdx uint64) (uint32, error)
	// MatchLiquidationOrder synthesises a victim-owned
	// LIQUIDATION_ORDER + IOC + reduce_only on the public orderbook
	// at `zeroPrice` and consumes opposite makers up to `baseAmount`.
	// Improvements above the zero-price floor are taxed at
	// `liquidationFeeBps` and routed to `liquidationFeeRecipient`
	// (LLP / Insurance Fund). Returns the filled base — IOC residue
	// is silently discarded.
	//
	// The synthetic taker is never persisted to the orderbook and
	// is not subject to authority / pool-account / pre-liquidation
	// gates that user-driven CreateOrder enforces. The matching loop
	// short-circuits the moment the victim is no longer in
	// PARTIAL/FULL liquidation health
	// (`is_not_in_liquidation_and_is_liquidation_order`).
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
