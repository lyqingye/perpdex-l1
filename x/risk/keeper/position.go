package keeper

import (
	"context"

	"cosmossdk.io/math"

	accounttypes "github.com/perpdex/perpdex-l1/x/account/types"
)

// position.go owns the per-position query helpers shared by x/trade and
// x/liquidation. They are *not* tied to the cross / isolated dichotomy
// — the trade keeper uses ComputePositionInitialMargin /
// ComputeUnrealizedPnLAt to size isolated margin deltas during a fill,
// while the liquidation keeper uses GetPositionMarkValue /
// GetPositionUnrealizedPnL to rank candidates for ADL & LLP takeover.

// ComputePositionInitialMargin returns the initial margin requirement
// for a HYPOTHETICAL position of |posAbs| in `marketIdx`, evaluated at
// the live mark price and the market's `default_initial_margin_fraction`.
//
// This is the in-Go equivalent of lighter's
// `position_requirement = position_abs * mark * quote_multiplier
// * margin_fraction_multiplier * IMF`. The trade keeper uses it to
// compute `margin_delta` for isolated positions during `ApplyPerpsMatching`
// without having to take a direct dependency on the oracle.
//
// `posAbs` MUST be non-negative — callers pre-compute |position|.
// Returns the IM in collateral units (math.Int).
func (k Keeper) ComputePositionInitialMargin(ctx context.Context, marketIdx uint32, posAbs math.Int) (math.Int, error) {
	if posAbs.IsNil() || posAbs.IsZero() {
		return math.ZeroInt(), nil
	}
	if posAbs.IsNegative() {
		posAbs = posAbs.Abs()
	}
	mark, err := k.resolveMarkPrice(ctx, marketIdx)
	if err != nil {
		return math.ZeroInt(), err
	}
	md, err := k.marketKeeper.GetMarketDetails(ctx, marketIdx)
	if err != nil {
		return math.ZeroInt(), err
	}
	// Build a synthetic position with |size|=posAbs so we can reuse
	// AccountPosition.InitialMargin (Notional * IMF / MarginTick).
	synthetic := accounttypes.AccountPosition{Position: posAbs}
	return synthetic.InitialMargin(mark, md), nil
}

// ComputeUnrealizedPnLAt returns the unrealized PnL for a HYPOTHETICAL
// position whose `position` (signed) and `entryQuote` are supplied
// directly, evaluated at the current mark price:
//
//	uPnL = position * mark - entry_quote
//
// This sister of `GetPositionUnrealizedPnL` operates on caller-supplied
// values so the trade keeper can reason about the pre/post-state of a
// position WITHIN the same fill (where the on-chain stored position has
// already been mutated to the post-state).
func (k Keeper) ComputeUnrealizedPnLAt(ctx context.Context, marketIdx uint32, position, entryQuote math.Int) (math.Int, error) {
	if position.IsNil() || position.IsZero() {
		return math.ZeroInt(), nil
	}
	mark, err := k.resolveMarkPrice(ctx, marketIdx)
	if err != nil {
		return math.ZeroInt(), err
	}
	if entryQuote.IsNil() {
		entryQuote = math.ZeroInt()
	}
	synthetic := accounttypes.AccountPosition{Position: position, EntryQuote: entryQuote}
	return synthetic.UnrealizedPnL(mark), nil
}

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
// separate so the package's split mirrors the way x/trade and
// x/liquidation actually consume them (hypothetical-position math vs
// stored-position math).
