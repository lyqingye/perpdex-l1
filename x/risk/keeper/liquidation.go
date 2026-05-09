package keeper

import (
	"context"
	"fmt"

	"cosmossdk.io/math"

	perptypes "github.com/perpdex/perpdex-l1/types"
	accounttypes "github.com/perpdex/perpdex-l1/x/account/types"
	markettypes "github.com/perpdex/perpdex-l1/x/market/types"
	"github.com/perpdex/perpdex-l1/x/risk/types"
)

// liquidation.go owns the math the liquidation keeper drives:
//
//   - GetPositionZeroPrice: gRPC entry point that returns the partial-
//     liquidation reference price ("zero price") for one (account,
//     market) pair. Mirrors the Lighter spec's `zero_price` formula.
//   - SimulateRiskAfterTakeover: previews the cross RiskParameters the
//     LLP / Insurance Fund would inherit if it absorbed `delta` of
//     `marketIdx`, so the LLP waterfall can short-circuit before
//     submitting a Msg.
//   - GetLiquidationRiskSnapshot: cohesive (account, market) bundle
//     consumed by the liquidation keeper's hot loops. Returns the
//     position, mark price, market details, the position's relevant
//     RiskParameters, the account's cross aggregate, and the pre-
//     computed zero price — everything those loops need without
//     re-querying or seeing the underlying formula.

// GetLiquidationRiskSnapshot returns the cohesive (pos, mark, md, Risk,
// CrossRisk, ZeroPrice) bundle for one (accountIdx, marketIdx) pair.
// `Risk` is the position's targeted envelope (cross aggregate or
// isolated per-position params); `CrossRisk` is always the account's
// cross aggregate so ADL ranking can keep using leverage on the cross
// aggregate even for isolated candidates.
//
// Each call performs ONE oracle read, ONE market read, ONE cross
// aggregation, and (for isolated targets only) ONE additional
// per-position aggregation, then folds the zero-price formula in. The
// returned snapshot is a value: it represents the state at the moment
// of the call and is invalidated by any subsequent mutation (fill,
// funding settlement, collateral move, oracle refresh).
func (k Keeper) GetLiquidationRiskSnapshot(
	ctx context.Context,
	accountIdx uint64,
	marketIdx uint32,
) (types.LiquidationRiskSnapshot, error) {
	pos, err := k.accountKeeper.GetPosition(ctx, accountIdx, marketIdx)
	if err != nil {
		return types.LiquidationRiskSnapshot{}, err
	}
	mark, md, err := k.GetMarkAndMarketDetails(ctx, marketIdx)
	if err != nil {
		return types.LiquidationRiskSnapshot{}, err
	}
	crossRi, err := k.ComputeRiskInfo(ctx, accountIdx)
	if err != nil {
		return types.LiquidationRiskSnapshot{}, err
	}
	var crossRP types.RiskParameters
	if crossRi.CurrentRiskParameters != nil {
		crossRP = *crossRi.CurrentRiskParameters
	}
	risk := crossRP
	if pos.MarginMode == perptypes.IsolatedMargin {
		rp, err := k.ComputeIsolatedRisk(ctx, accountIdx, marketIdx)
		if err != nil {
			return types.LiquidationRiskSnapshot{}, err
		}
		risk = rp
	}
	zp := pureComputeZeroPrice(pos, mark, md, risk.TotalAccountValue, risk.MaintenanceMarginRequirement)
	return types.LiquidationRiskSnapshot{
		Position:      pos,
		MarkPrice:     mark,
		MarketDetails: md,
		Risk:          risk,
		CrossRisk:     crossRP,
		ZeroPrice:     zp,
	}, nil
}

// pureComputeZeroPrice is the package-private zero-price formula. The
// returned uint32 satisfies (0, MaxOrderPrice]; zero-position and
// zero-mark short-circuit to 0 because the caller is expected to
// detect those cases before quoting a price.
//
// The formula is:
//
//	zeroPrice_long  = mark * (1 - sign(pos) * M_i * TAV / MMR)
//	zeroPrice_short = mark * (1 + |sign(pos)| * M_i * TAV / MMR)
//
// where `M_i` is `md.MaintenanceMarginFraction` (basis points /
// MarginTick) and `tav` / `mmr` are the relevant scope's totals
// (cross aggregate or isolated per-position).
func pureComputeZeroPrice(
	pos accounttypes.AccountPosition,
	mark uint32,
	md markettypes.MarketDetails,
	tav, mmr math.Int,
) uint32 {
	if pos.Position.IsZero() || mark == 0 {
		return 0
	}
	markBig := math.NewIntFromUint64(uint64(mark))
	// Degenerate case: no maintenance requirement (only happens when
	// the position has been fully closed — guarded above — or for
	// malformed market configs). Fall back to the mark.
	if mmr.IsZero() {
		return mark
	}
	mi := math.NewIntFromUint64(uint64(md.MaintenanceMarginFraction))
	tickBig := math.NewIntFromUint64(uint64(perptypes.MarginTick))
	num := markBig.Mul(mi).Mul(tav)
	denom := mmr.Mul(tickBig)
	adjustment := quoTowardZero(num, denom)

	var zp math.Int
	if pos.Position.IsNegative() {
		// Short: zeroPrice = mark * (1 + M·TAV/MMR).
		zp = markBig.Add(adjustment)
	} else {
		// Long: zeroPrice = mark * (1 - M·TAV/MMR).
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

// GetPositionZeroPrice returns the price at which liquidating a
// portion of the position would leave the account's TAV/MMR ratio
// invariant — the "zero price" defined in the Lighter spec. Bankrupt
// accounts (TAV < 0) are not partially liquidatable; callers must
// short-circuit before invoking this.
//
// Public entry point used by the gRPC query path. The hot liquidation
// loops use `GetLiquidationRiskSnapshot` instead so the snapshot's
// other fields (Risk / CrossRisk / mark / md) are not thrown away.
func (k Keeper) GetPositionZeroPrice(ctx context.Context, accountIdx uint64, marketIdx uint32) (uint32, error) {
	snap, err := k.GetLiquidationRiskSnapshot(ctx, accountIdx, marketIdx)
	if err != nil {
		return 0, err
	}
	return snap.ZeroPrice, nil
}

// quoTowardZero divides `num/denom` rounding toward zero so that signed
// adjustments behave symmetrically (math.Int.Quo uses Go-style
// truncated division which already truncates toward zero, but we wrap
// it for clarity and to make the intent explicit when num is negative).
func quoTowardZero(num, denom math.Int) math.Int {
	if denom.IsZero() {
		return math.ZeroInt()
	}
	return num.Quo(denom)
}

// SimulateRiskAfterTakeover computes what the account's CROSS risk
// parameters would look like if `delta` (signed base size) of
// `marketIdx` were ADDED to the account's existing position at
// `entryPrice`. Used by the LLP / insurance-fund take-over routine to
// preview whether absorbing a victim's position would push the LLP
// below its initial-margin requirement.
//
// `entryPrice` is the price at which the takeover would be settled
// (typically the victim's zero price). `delta` carries the sign of
// the position the LLP would inherit.
//
// Refusing isolated targets here is intentional: LLP and Insurance
// Fund positions MUST be cross-margined (the pool acts as the cross
// counterparty by mandate). An isolated position on the LLP indicates
// a misconfiguration upstream; we surface it as an error so the
// EndBlocker logs it instead of silently mis-simulating the takeover.
func (k Keeper) SimulateRiskAfterTakeover(
	ctx context.Context,
	accountIdx uint64,
	marketIdx uint32,
	delta math.Int,
	entryPrice uint32,
) (types.RiskParameters, error) {
	base, err := k.ComputeRiskInfo(ctx, accountIdx)
	if err != nil {
		return types.RiskParameters{}, err
	}
	cur := types.RiskParameters{}
	if base.CurrentRiskParameters != nil {
		cur = *base.CurrentRiskParameters
	}
	if delta.IsZero() {
		return cur, nil
	}
	pos, err := k.accountKeeper.GetPosition(ctx, accountIdx, marketIdx)
	if err != nil {
		return types.RiskParameters{}, err
	}
	if pos.MarginMode == perptypes.IsolatedMargin {
		return types.RiskParameters{}, fmt.Errorf("simulate_takeover: account %d market %d is isolated", accountIdx, marketIdx)
	}
	mark, md, err := k.GetMarkAndMarketDetails(ctx, marketIdx)
	if err != nil {
		return types.RiskParameters{}, err
	}
	return pureApplySimulatedTakeover(pos, cur, mark, md, delta, entryPrice), nil
}

// pureApplySimulatedTakeover folds `delta` of `pos` (settled at
// `entryPrice`) into a starting cross aggregate `current` and returns
// the would-be post-takeover RiskParameters. No state is mutated; the
// post-state is driven through the canonical `pos.ApplyFill` so the
// simulation cannot drift from the engine's settlement maths.
func pureApplySimulatedTakeover(
	pos accounttypes.AccountPosition,
	current types.RiskParameters,
	mark uint32,
	md markettypes.MarketDetails,
	delta math.Int,
	entryPrice uint32,
) types.RiskParameters {
	if delta.IsZero() {
		return current
	}
	cur := current
	// Subtract the OLD contribution of (account, market) from cur.
	if !pos.Position.IsZero() {
		cur.InitialMarginRequirement = cur.InitialMarginRequirement.Sub(pos.InitialMargin(mark, md))
		cur.MaintenanceMarginRequirement = cur.MaintenanceMarginRequirement.Sub(pos.MaintenanceMargin(mark, md))
		cur.CloseOutMarginRequirement = cur.CloseOutMarginRequirement.Sub(pos.CloseOutMargin(mark, md))
		cur.TotalAccountValue = cur.TotalAccountValue.Sub(pos.UnrealizedPnL(mark))
	}
	// Apply the simulated takeover via the canonical fill helper. This
	// shares the four-quadrant entry_quote logic with x/trade so the
	// simulation cannot drift from the actual settlement maths.
	res := pos.ApplyFill(delta, entryPrice)
	newPos := res.Position
	if !newPos.Position.IsZero() {
		cur.InitialMarginRequirement = cur.InitialMarginRequirement.Add(newPos.InitialMargin(mark, md))
		cur.MaintenanceMarginRequirement = cur.MaintenanceMarginRequirement.Add(newPos.MaintenanceMargin(mark, md))
		cur.CloseOutMarginRequirement = cur.CloseOutMarginRequirement.Add(newPos.CloseOutMargin(mark, md))
		cur.TotalAccountValue = cur.TotalAccountValue.Add(newPos.UnrealizedPnL(mark))
	}
	return cur
}
