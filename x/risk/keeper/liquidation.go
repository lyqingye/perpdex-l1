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

// liquidation.go owns the math the liquidation keeper drives directly:
//
//   - GetPositionZeroPrice returns the partial-liquidation reference
//     price ("zero price") that keeper-bot driven `MsgLiquidate` quotes.
//   - ComputeZeroPrice is the pure math used by GetPositionZeroPrice;
//     callers that already hold (pos, mark, md, tav, mmr) skip the
//     re-fetch overhead by invoking the helper directly.
//   - SimulateRiskAfterTakeover previews the cross-account RiskParameters
//     a pool / IF would inherit if it absorbed `delta` of `marketIdx`,
//     so the LLP waterfall can short-circuit before submitting a Msg.
//   - ApplySimulatedTakeover is the pure math used by
//     SimulateRiskAfterTakeover; same dedup story for callers that
//     already hold the inputs.
//   - quoTowardZero is a tiny shared division helper used by both.
//
// Cohesive grouping with risk_change / cross / isolated would have
// either polluted those files with liquidation-specific knowledge
// (zero price formula, takeover simulation) or required scattering
// liquidation entry points across all of them. Split to keep both
// halves narrowly scoped.

// ComputeZeroPrice is the pure math behind GetPositionZeroPrice.
// Exposed as a Keeper method (with no state access) so it can be
// declared on the liquidation RiskKeeper interface and overridden by
// test stubs; the real implementation forwards to the package-level
// pureComputeZeroPrice and adds zero IO.
//
// The formula is:
//
//	zeroPrice_long  = mark * (1 - sign(pos) * M_i * TAV / MMR)
//	zeroPrice_short = mark * (1 + |sign(pos)| * M_i * TAV / MMR)
//
// where `M_i` is `md.MaintenanceMarginFraction` (basis points /
// MarginTick) and `tav` / `mmr` are the relevant scope's totals
// (cross aggregate or isolated per-position). The returned uint32 is
// clamped to (0, MaxOrderPrice]; zero positions and bankrupt
// accounts are the caller's responsibility to short-circuit.
//
// liquidation-side callers that already hold (pos, mark, md, tav,
// mmr) — typically because they just ran ComputeRiskInfo /
// ComputeIsolatedRisk for unrelated reasons — invoke this directly
// via the RiskKeeper interface to skip the GetPositionZeroPrice
// wrapper's redundant pos / mark / md / risk-info round-trips.
func (k Keeper) ComputeZeroPrice(
	pos accounttypes.AccountPosition,
	mark uint32,
	md markettypes.MarketDetails,
	tav, mmr math.Int,
) uint32 {
	return pureComputeZeroPrice(pos, mark, md, tav, mmr)
}

// pureComputeZeroPrice is the package-private formula shared by
// ComputeZeroPrice and the GetPositionZeroPrice wrapper.
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

// GetPositionZeroPrice returns the price at which liquidating a portion
// of the position would leave the account's TAV/MMR ratio invariant —
// i.e. the "zero price" defined in the Lighter spec. Bankrupt accounts
// (TAV < 0) are not partially liquidatable; callers must short-circuit
// before invoking this.
//
// Thin wrapper: gathers (pos, mark, md, tav, mmr) and delegates to
// ComputeZeroPrice. liquidation-side callers that already hold these
// values should skip this and call ComputeZeroPrice directly.
func (k Keeper) GetPositionZeroPrice(ctx context.Context, accountIdx uint64, marketIdx uint32) (uint32, error) {
	pos, err := k.accountKeeper.GetPosition(ctx, accountIdx, marketIdx)
	if err != nil {
		return 0, err
	}
	if pos.Position.IsZero() {
		return 0, nil
	}
	mark, md, err := k.GetMarkAndMarketDetails(ctx, marketIdx)
	if err != nil {
		return 0, err
	}

	var tav, mmr math.Int
	if pos.MarginMode == perptypes.IsolatedMargin {
		rp, err := k.ComputeIsolatedRisk(ctx, accountIdx, marketIdx)
		if err != nil {
			return 0, err
		}
		tav = rp.TotalAccountValue
		mmr = rp.MaintenanceMarginRequirement
	} else {
		ri, err := k.ComputeRiskInfo(ctx, accountIdx)
		if err != nil {
			return 0, err
		}
		if ri.CrossRiskParameters == nil {
			return mark, nil
		}
		tav = ri.CrossRiskParameters.TotalAccountValue
		mmr = ri.CrossRiskParameters.MaintenanceMarginRequirement
	}
	return pureComputeZeroPrice(pos, mark, md, tav, mmr), nil
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

// ApplySimulatedTakeover is the pure math behind
// SimulateRiskAfterTakeover. Exposed as a Keeper method (with no
// state access) so it can be declared on the liquidation RiskKeeper
// interface and overridden by test stubs; the real implementation
// forwards to the package-level pureApplySimulatedTakeover.
//
// `current` is the caller's pre-fetched CROSS RiskParameters (from
// ComputeRiskInfo.CurrentRiskParameters); `pos` is the targeted
// (account, market) position; mark/md/delta/entryPrice mirror the
// wrapper.
//
// The simulation ONLY updates the targeted position's |size| and
// entry_quote contribution to IM/MM/CM/uPnL; it does NOT mutate any
// state. Returned RiskParameters are the would-be cross aggregates.
//
// Internally we drive the post-state through `pos.ApplyFill` so the
// four-quadrant entry_quote arithmetic stays in lockstep with
// `applyPositionChange` (single source of truth on lighter parity).
//
// liquidation-side callers that already hold (pos, current, mark, md)
// invoke this directly via the RiskKeeper interface to skip the
// SimulateRiskAfterTakeover wrapper's ComputeRiskInfo + GetPosition +
// GetMarkAndMarketDetails round-trips.
func (k Keeper) ApplySimulatedTakeover(
	pos accounttypes.AccountPosition,
	current types.RiskParameters,
	mark uint32,
	md markettypes.MarketDetails,
	delta math.Int,
	entryPrice uint32,
) types.RiskParameters {
	return pureApplySimulatedTakeover(pos, current, mark, md, delta, entryPrice)
}

// pureApplySimulatedTakeover is the package-private formula shared by
// ApplySimulatedTakeover and the SimulateRiskAfterTakeover wrapper.
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

// SimulateRiskAfterTakeover computes what the account's CROSS risk
// parameters would look like if `delta` (signed base size) of `marketIdx`
// were ADDED to the account's existing position at `entryPrice`. This
// is used by the LLP/insurance-fund take-over routine to preview
// whether absorbing a victim's position would push the LLP below its
// initial margin requirement.
//
// `entryPrice` is the price at which the takeover would be settled
// (typically the victim's zero price). `delta` carries the sign of the
// position the LLP would inherit.
//
// Thin wrapper: gathers (pos, current, mark, md) and delegates to
// ApplySimulatedTakeover. liquidation-side callers that already hold
// these values should skip this and call ApplySimulatedTakeover
// directly.
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
		// LLP / IF positions are always cross-margined; refusing here
		// surfaces the misconfiguration.
		return types.RiskParameters{}, fmt.Errorf("simulate_takeover: account %d market %d is isolated", accountIdx, marketIdx)
	}
	mark, md, err := k.GetMarkAndMarketDetails(ctx, marketIdx)
	if err != nil {
		return types.RiskParameters{}, err
	}
	return pureApplySimulatedTakeover(pos, cur, mark, md, delta, entryPrice), nil
}
