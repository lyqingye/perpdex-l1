package keeper

import (
	"context"

	"cosmossdk.io/math"
)

// position.go owns the per-position query helpers consumed by the
// liquidation keeper to rank ADL / LLP candidates. The trade keeper
// previously sourced its hypothetical-position IM / uPnL from this
// file too (`ComputePositionInitialMargin` / `ComputeUnrealizedPnLAt`),
// but those helpers retired in favour of the cohesive split:
//
//   - `RiskKeeper.GetMarkAndMarketDetails` returns mark + md in one
//     round-trip;
//   - `MarketDetails.InitialMargin(posAbs, mark)` owns the IM formula
//     (the IMF multiplier lives on MarketDetails, so the math is
//     hosted there alongside MaintenanceMargin / CloseOutMargin's
//     siblings on AccountPosition);
//   - `AccountPosition.UnrealizedPnL(mark)` covers the uPnL math.
//
// Trade-side `calculateIsolatedMarginDelta` now drives those primitives
// directly, freeing this file to focus on the stored-position queries
// liquidation actually consumes.

// GetPositionMarkValue returns |position| * mark_price as a math.Int.
// Returns zero when no position exists; errors out on missing/stale oracle.
func (k Keeper) GetPositionMarkValue(ctx context.Context, accountIdx uint64, marketIdx uint32) (math.Int, error) {
	pos, err := k.accountKeeper.GetPosition(ctx, accountIdx, marketIdx)
	if err != nil {
		return math.ZeroInt(), err
	}
	if pos.Position.IsZero() {
		return math.ZeroInt(), nil
	}
	mark, err := k.resolveMarkPrice(ctx, marketIdx)
	if err != nil {
		return math.ZeroInt(), err
	}
	return pos.Notional(mark), nil
}

// GetPositionUnrealizedPnL returns the signed unrealized PnL of the
// (account, market) position at the current mark price:
//
//	uPnL = position * mark_price - entry_quote
//
// Positive when the position is in profit. Returns zero when no position
// exists or no mark price is available.
func (k Keeper) GetPositionUnrealizedPnL(ctx context.Context, accountIdx uint64, marketIdx uint32) (math.Int, error) {
	pos, err := k.accountKeeper.GetPosition(ctx, accountIdx, marketIdx)
	if err != nil {
		return math.ZeroInt(), err
	}
	if pos.Position.IsZero() {
		return math.ZeroInt(), nil
	}
	mark, err := k.resolveMarkPrice(ctx, marketIdx)
	if err != nil {
		return math.ZeroInt(), err
	}
	return pos.UnrealizedPnL(mark), nil
}

// note: ComputeRiskInfo / ComputeIsolatedRisk live in cross.go and
// isolated.go respectively; the per-position helpers above are kept
// separate so the package's split mirrors the way x/liquidation
// consumes them (stored-position queries for ADL / LLP ranking).
