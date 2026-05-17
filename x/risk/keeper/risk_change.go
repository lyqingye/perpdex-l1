package keeper

import (
	"context"

	perptypes "github.com/perpdex/perpdex-l1/types"
	accounttypes "github.com/perpdex/perpdex-l1/x/account/types"
	"github.com/perpdex/perpdex-l1/x/risk/types"
)

// risk_change.go stitches the per-scope decision functions
// (cross.isCrossRiskChangeValid + isolated.isIsolatedRiskChangeValid)
// into the account-wide regression check. Pre-state is function-local
// (PreRiskSnapshot threaded through callers); a chain-level cache
// would leak pre-state across handlers.

// classifyChange is the shared pre-vs-post decision used by both
// scopes. missingPre rejects any unhealthy post-state to avoid
// silently accepting a regression that may have introduced it.
func classifyChange(pre, post types.RiskParameters, missingPre bool) bool {
	postClass := post.HealthStatus()
	if postClass == perptypes.HealthHealthy {
		return true
	}
	if missingPre {
		return false
	}
	preClass := pre.HealthStatus()
	if postClass > preClass {
		return false
	}
	switch preClass {
	case perptypes.HealthPreLiquidation:
		// PRE rule: no MMR growth and TAV/MMR ratio non-decreasing.
		// Mark is constant within a block, so the MMR cap is
		// equivalent to a per-position |size| cap.
		if post.MaintenanceMarginRequirement.GT(pre.MaintenanceMarginRequirement) {
			return false
		}
		if pre.MaintenanceMarginRequirement.IsZero() ||
			post.MaintenanceMarginRequirement.IsZero() {
			return true
		}
		// post.TAV/post.MMR >= pre.TAV/pre.MMR, cross-multiplied so
		// integer truncation cannot flatter the ratio.
		lhs := post.TotalAccountValue.Mul(pre.MaintenanceMarginRequirement)
		rhs := pre.TotalAccountValue.Mul(post.MaintenanceMarginRequirement)
		return !lhs.LT(rhs)
	default:
		// PARTIAL/FULL/BANKRUPTCY pre: enforce TAV/IM coverage so
		// liquidation fills can never worsen capital efficiency.
		// Once below MMR, IMR coverage is the stricter benchmark.
		if post.InitialMarginRequirement.IsZero() ||
			pre.InitialMarginRequirement.IsZero() {
			return true
		}
		lhs := post.TotalAccountValue.Mul(pre.InitialMarginRequirement)
		rhs := pre.TotalAccountValue.Mul(post.InitialMarginRequirement)
		return !lhs.LT(rhs)
	}
}

// IsValidRiskChangeFrom enforces post-vs-pre risk invariants across
// the cross aggregate and every isolated position. Either side
// regressing rejects the change.
//
//   - HEALTHY post: accepted unconditionally.
//   - PRE pre: post.class <= PRE, post.MMR <= pre.MMR, TAV/MMR ratio
//     cannot decrease.
//   - PARTIAL/FULL/BANKRUPTCY pre: post.class <= pre.class and
//     TAV/IM ratio cannot decrease (safety net for liquidation fills).
//
// pre MUST come from SnapshotRisk in the same handler. A zero-value
// snapshot is treated as "no pre-state" → post must be HEALTHY.
func (k Keeper) IsValidRiskChangeFrom(ctx context.Context, accountIdx uint64, pre types.PreRiskSnapshot) (bool, error) {
	if ok, err := k.isCrossRiskChangeValid(ctx, accountIdx, pre.Cross); err != nil || !ok {
		return ok, err
	}
	// Missing isolated pre is not an error — the position may have
	// just been opened in this fill; fall back to "post must be
	// HEALTHY".
	var (
		ok      = true
		iterErr error
	)
	if err := k.accountKeeper.IterateAccountPositions(ctx, accountIdx, func(pos accounttypes.AccountPosition) bool {
		if pos.BaseSize.IsZero() || pos.MarginMode != perptypes.IsolatedMargin {
			return false
		}
		preIso, hasPre := pre.IsolatedFor(pos.MarketIndex)
		valid, err := k.isIsolatedRiskChangeValid(ctx, accountIdx, pos.MarketIndex, preIso, hasPre)
		if err != nil {
			iterErr = err
			ok = false
			return true
		}
		if !valid {
			ok = false
			return true
		}
		return false
	}); err != nil {
		return false, err
	}
	if iterErr != nil {
		return false, iterErr
	}
	return ok, nil
}

// SnapshotRisk captures the cross aggregate and every non-zero
// isolated position by value. Markets the account does not currently
// hold are intentionally absent so IsValidRiskChangeFrom falls back
// to "post must be HEALTHY" for positions opened during the handler.
func (k Keeper) SnapshotRisk(ctx context.Context, accountIdx uint64) (types.PreRiskSnapshot, error) {
	snap := types.PreRiskSnapshot{}
	cross, err := k.ComputeCrossRisk(ctx, accountIdx)
	if err != nil {
		return types.PreRiskSnapshot{}, err
	}
	snap.Cross = &cross
	var iterErr error
	if err := k.accountKeeper.IterateAccountPositions(ctx, accountIdx, func(pos accounttypes.AccountPosition) bool {
		if pos.BaseSize.IsZero() || pos.MarginMode != perptypes.IsolatedMargin {
			return false
		}
		rp, err := k.ComputeIsolatedRisk(ctx, accountIdx, pos.MarketIndex)
		if err != nil {
			iterErr = err
			return true
		}
		if snap.Isolated == nil {
			snap.Isolated = map[uint32]types.RiskParameters{}
		}
		snap.Isolated[pos.MarketIndex] = rp
		return false
	}); err != nil {
		return types.PreRiskSnapshot{}, err
	}
	if iterErr != nil {
		return types.PreRiskSnapshot{}, iterErr
	}
	return snap, nil
}
