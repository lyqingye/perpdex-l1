package keeper

import (
	"context"

	"cosmossdk.io/math"

	perptypes "github.com/perpdex/perpdex-l1/types"
	accounttypes "github.com/perpdex/perpdex-l1/x/account/types"
	markettypes "github.com/perpdex/perpdex-l1/x/market/types"
	"github.com/perpdex/perpdex-l1/x/risk/types"
)

// GetLiquidationRiskSnapshot returns the cohesive (pos, mark, md,
// Risk, CrossRisk, ZeroPrice) bundle for one (account, market). Risk
// is the position's targeted envelope (cross or isolated); CrossRisk
// is always the account's cross aggregate so ADL ranking can drive
// leverage off it even for isolated candidates.
//
// Snapshots are values and MUST be rebuilt after any state mutation
// (fill, funding settle, collateral move, oracle refresh) — threading
// across a mutation feeds stale TAV/MMR into the next step.
//
// Closed positions short-circuit to a zero snapshot without an
// oracle read, preserving the gRPC zero-price "0 on empty" semantics
// and letting Liquidate / Deleverage report "no position" before any
// oracle dependency can fail.
func (k Keeper) GetLiquidationRiskSnapshot(
	ctx context.Context,
	accountIdx uint64,
	marketIdx uint32,
) (types.LiquidationRiskSnapshot, error) {
	pos, err := k.accountKeeper.GetPosition(ctx, accountIdx, marketIdx)
	if err != nil {
		return types.LiquidationRiskSnapshot{}, err
	}
	if pos.BaseSize.IsZero() {
		return types.LiquidationRiskSnapshot{Position: pos}, nil
	}
	markPrice, md, err := k.marketKeeper.GetMarkPriceAndDetails(ctx, marketIdx)
	if err != nil {
		return types.LiquidationRiskSnapshot{}, err
	}
	crossRP, err := k.ComputeCrossRisk(ctx, accountIdx)
	if err != nil {
		return types.LiquidationRiskSnapshot{}, err
	}
	risk := crossRP
	if pos.MarginMode == perptypes.IsolatedMargin {
		rp, err := k.ComputeIsolatedRisk(ctx, accountIdx, marketIdx)
		if err != nil {
			return types.LiquidationRiskSnapshot{}, err
		}
		risk = rp
	}
	zp := pureComputeZeroPrice(pos, markPrice, md, risk.TotalAccountValue, risk.MaintenanceMarginRequirement)
	return types.LiquidationRiskSnapshot{
		Position:      pos,
		MarkPrice:     markPrice,
		MarketDetails: md,
		Risk:          risk,
		CrossRisk:     crossRP,
		ZeroPrice:     zp,
	}, nil
}

// pureComputeZeroPrice is the package-private zero-price formula:
//
//	zp_long  = markPrice * (1 - M_i * TAV / MMR)
//	zp_short = markPrice * (1 + M_i * TAV / MMR)
//
// where M_i is md.MaintenanceMarginFraction (in MarginTick) and
// tav / mmr come from the relevant scope. Returns in (0, MaxOrderPrice];
// zero position / zero mark short-circuit to 0.
func pureComputeZeroPrice(
	pos accounttypes.AccountPosition,
	markPrice uint32,
	md markettypes.MarketDetails,
	tav, mmr math.Int,
) uint32 {
	if pos.BaseSize.IsZero() || markPrice == 0 {
		return 0
	}
	markBig := math.NewIntFromUint64(uint64(markPrice))
	// Degenerate case: no maintenance requirement (malformed market
	// config or fully-closed position, already guarded). Fall back to
	// markPrice.
	if mmr.IsZero() {
		return markPrice
	}
	mi := math.NewIntFromUint64(uint64(md.MaintenanceMarginFraction))
	tickBig := math.NewIntFromUint64(uint64(perptypes.MarginTick))
	num := markBig.Mul(mi).Mul(tav)
	denom := mmr.Mul(tickBig)
	adjustment := quoTowardZero(num, denom)

	var zp math.Int
	if pos.IsShort() {
		zp = markBig.Add(adjustment)
	} else {
		zp = markBig.Sub(adjustment)
	}
	if zp.IsNegative() || zp.IsZero() {
		return 1
	}
	maxPrice := math.NewIntFromUint64(uint64(perptypes.MaxOrderPrice))
	if zp.GT(maxPrice) {
		return perptypes.MaxOrderPrice
	}
	return uint32(zp.Uint64())
}

// GetPositionZeroPrice returns the partial-liquidation reference
// price (TAV/MMR-invariant) for a position. Bankrupt accounts (TAV
// < 0) are not partially liquidatable — callers short-circuit first.
// gRPC entry point; ADL hot loops use GetLiquidationRiskSnapshot to
// keep Risk / CrossRisk / mark / md.
func (k Keeper) GetPositionZeroPrice(ctx context.Context, accountIdx uint64, marketIdx uint32) (uint32, error) {
	snap, err := k.GetZeroPriceSnapshot(ctx, accountIdx, marketIdx)
	if err != nil {
		return 0, err
	}
	return snap.ZeroPrice, nil
}

// GetZeroPriceSnapshot is the lightweight companion to
// GetLiquidationRiskSnapshot for callers that only need (Position,
// ZeroPrice) — the gRPC zero-price query and the Liquidate /
// Deleverage handlers. Skips CrossRisk / MarketDetails. Empty
// positions short-circuit to ZP=0 without touching the oracle.
func (k Keeper) GetZeroPriceSnapshot(
	ctx context.Context,
	accountIdx uint64,
	marketIdx uint32,
) (types.ZeroPriceSnapshot, error) {
	pos, err := k.accountKeeper.GetPosition(ctx, accountIdx, marketIdx)
	if err != nil {
		return types.ZeroPriceSnapshot{}, err
	}
	if pos.BaseSize.IsZero() {
		return types.ZeroPriceSnapshot{Position: pos}, nil
	}
	markPrice, md, err := k.marketKeeper.GetMarkPriceAndDetails(ctx, marketIdx)
	if err != nil {
		return types.ZeroPriceSnapshot{}, err
	}
	var risk types.RiskParameters
	if pos.MarginMode == perptypes.IsolatedMargin {
		rp, err := k.ComputeIsolatedRisk(ctx, accountIdx, marketIdx)
		if err != nil {
			return types.ZeroPriceSnapshot{}, err
		}
		risk = rp
	} else {
		rp, err := k.ComputeCrossRisk(ctx, accountIdx)
		if err != nil {
			return types.ZeroPriceSnapshot{}, err
		}
		risk = rp
	}
	zp := pureComputeZeroPrice(pos, markPrice, md, risk.TotalAccountValue, risk.MaintenanceMarginRequirement)
	return types.ZeroPriceSnapshot{Position: pos, ZeroPrice: zp}, nil
}

// quoTowardZero wraps math.Int.Quo (truncated division) so the
// "truncate toward zero on negative numerator" intent is explicit.
func quoTowardZero(num, denom math.Int) math.Int {
	if denom.IsZero() {
		return math.ZeroInt()
	}
	return num.Quo(denom)
}

// SimulateRiskAfterTakeover previews the account's CROSS risk after
// adding delta (signed base) at entryPrice. Used by the LLP/IF
// takeover routine to check that absorbing a victim's position does
// not breach the pool's IMR.
//
// Refusing isolated targets is intentional: LLP / IF positions MUST
// be cross-margined; an isolated target indicates an upstream
// misconfiguration and surfaces as an error rather than silently
// mis-simulating.
func (k Keeper) SimulateRiskAfterTakeover(
	ctx context.Context,
	accountIdx uint64,
	marketIdx uint32,
	delta math.Int,
	entryPrice uint32,
) (types.RiskParameters, error) {
	cur, err := k.ComputeCrossRisk(ctx, accountIdx)
	if err != nil {
		return types.RiskParameters{}, err
	}
	if delta.IsZero() {
		return cur, nil
	}
	pos, err := k.accountKeeper.GetPosition(ctx, accountIdx, marketIdx)
	if err != nil {
		return types.RiskParameters{}, err
	}
	if pos.MarginMode == perptypes.IsolatedMargin {
		return types.RiskParameters{}, accounttypes.ErrInvalidMarginMode.Wrapf(
			"simulate_takeover requires cross margin: account %d market %d is isolated",
			accountIdx, marketIdx,
		)
	}
	markPrice, md, err := k.marketKeeper.GetMarkPriceAndDetails(ctx, marketIdx)
	if err != nil {
		return types.RiskParameters{}, err
	}
	return pureApplySimulatedTakeover(pos, cur, markPrice, md, delta, entryPrice), nil
}

// pureApplySimulatedTakeover folds delta of pos (settled at
// entryPrice) into current and returns the post-takeover params. No
// state mutation; drives the post-state through pos.ApplyFill so the
// simulation cannot drift from the engine's settlement math.
func pureApplySimulatedTakeover(
	pos accounttypes.AccountPosition,
	current types.RiskParameters,
	markPrice uint32,
	md markettypes.MarketDetails,
	delta math.Int,
	entryPrice uint32,
) types.RiskParameters {
	if delta.IsZero() {
		return current
	}
	cur := current
	// Remove the OLD (account, market) contribution from cur.
	if !pos.BaseSize.IsZero() {
		cur.InitialMarginRequirement = cur.InitialMarginRequirement.Sub(pos.InitialMargin(markPrice, md))
		cur.MaintenanceMarginRequirement = cur.MaintenanceMarginRequirement.Sub(pos.MaintenanceMargin(markPrice, md))
		cur.CloseOutMarginRequirement = cur.CloseOutMarginRequirement.Sub(pos.CloseOutMargin(markPrice, md))
		cur.TotalAccountValue = cur.TotalAccountValue.Sub(pos.UnrealizedPnL(markPrice))
	}
	// Apply via the canonical pos.ApplyFill so simulation shares the
	// four-quadrant entry_quote logic with x/trade.
	res := pos.ApplyFill(delta, entryPrice)
	newPos := res.Position
	if !newPos.BaseSize.IsZero() {
		cur.InitialMarginRequirement = cur.InitialMarginRequirement.Add(newPos.InitialMargin(markPrice, md))
		cur.MaintenanceMarginRequirement = cur.MaintenanceMarginRequirement.Add(newPos.MaintenanceMargin(markPrice, md))
		cur.CloseOutMarginRequirement = cur.CloseOutMarginRequirement.Add(newPos.CloseOutMargin(markPrice, md))
		cur.TotalAccountValue = cur.TotalAccountValue.Add(newPos.UnrealizedPnL(markPrice))
	}
	return cur
}
