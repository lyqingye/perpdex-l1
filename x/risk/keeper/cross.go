package keeper

import (
	"context"
	"errors"
	"fmt"

	"cosmossdk.io/math"

	perptypes "github.com/perpdex/perpdex-l1/types"
	accounttypes "github.com/perpdex/perpdex-l1/x/account/types"
	"github.com/perpdex/perpdex-l1/x/risk/types"
)

// cross.go owns the cross-margin half of the risk keeper: the aggregate
// ComputeRiskInfo walk + the *pure read* helpers that surface its
// outputs (GetHealthStatus / GetTotalAccountValue /
// GetAvailableCollateral / GetAvailableUsdcCollateral). The
// per-account half of IsValidRiskChangeFrom (isCrossRiskChangeValid)
// lives here too because it consumes the same aggregates; the
// cross + isolated driver IsValidRiskChangeFrom and SnapshotRisk live
// in risk_change.go.

// ComputeRiskInfo iterates all CROSS positions of an account and aggregates
// their risk contributions into a RiskInfo struct.
//
// Per the spec, isolated positions are a separate accounting unit: the
// allocated margin of an isolated position only collateralises that
// position, and its uPnL is realised against AllocatedMargin (not the
// shared cross collateral). Including isolated AllocatedMargin/uPnL in
// the cross TAV — without also adding the corresponding IM/MM/CM — let
// an isolated profit silently inflate cross health and dodge cross
// liquidation. We therefore aggregate ONLY cross-margin positions here;
// isolated positions are evaluated individually via ComputeIsolatedRisk.
func (k Keeper) ComputeRiskInfo(ctx context.Context, accountIdx uint64) (types.RiskInfo, error) {
	a, err := k.accountKeeper.GetAccount(ctx, accountIdx)
	if err != nil {
		return types.RiskInfo{}, err
	}
	collateral := a.Collateral

	cross := types.RiskParameters{
		Collateral:                   collateral,
		CollateralWithFunding:        collateral,
		TotalAccountValue:            collateral,
		InitialMarginRequirement:     math.ZeroInt(),
		MaintenanceMarginRequirement: math.ZeroInt(),
		CloseOutMarginRequirement:    math.ZeroInt(),
	}

	imSum := math.ZeroInt()
	mmSum := math.ZeroInt()
	cmSum := math.ZeroInt()
	totalCross := collateral

	var iterErr error
	if err := k.accountKeeper.IterateAccountPositions(ctx, accountIdx, func(pos accounttypes.AccountPosition) bool {
		if pos.BaseSize.IsZero() {
			return false
		}
		// Skip isolated positions: they have an independent risk
		// envelope that ComputeIsolatedRisk evaluates on demand.
		if pos.MarginMode == perptypes.IsolatedMargin {
			return false
		}
		// For any NON-ZERO position the oracle must return a fresh,
		// non-zero mark. Silently skipping a missing price previously
		// made bankrupt accounts look healthy whenever the oracle
		// hiccupped. Fail-closed keeps the invariant "risk regression
		// cannot be hidden by an oracle outage".
		mark, err := k.resolveMarkPrice(ctx, pos.MarketIndex)
		if err != nil {
			iterErr = errors.Join(err, fmt.Errorf("account=%d", accountIdx))
			return true
		}
		md, err := k.marketKeeper.GetMarketDetails(ctx, pos.MarketIndex)
		if err != nil {
			iterErr = err
			return true
		}
		imSum = imSum.Add(pos.InitialMargin(mark, md))
		mmSum = mmSum.Add(pos.MaintenanceMargin(mark, md))
		cmSum = cmSum.Add(pos.CloseOutMargin(mark, md))
		totalCross = totalCross.Add(pos.UnrealizedPnL(mark))
		return false
	}); err != nil {
		return types.RiskInfo{}, err
	}
	if iterErr != nil {
		return types.RiskInfo{}, iterErr
	}

	cross.TotalAccountValue = totalCross
	cross.InitialMarginRequirement = imSum
	cross.MaintenanceMarginRequirement = mmSum
	cross.CloseOutMarginRequirement = cmSum

	// Both cross_risk_parameters and current_risk_parameters describe
	// the cross account. Isolated positions are queried separately via
	// ComputeIsolatedRisk / GetIsolatedHealthStatus. Returning the
	// same pointer twice would let downstream callers mutate one and
	// surprise the other; we deep-copy via two struct values.
	current := cross
	return types.RiskInfo{CrossRiskParameters: &cross, CurrentRiskParameters: &current}, nil
}

// GetHealthStatus returns the CROSS health status. Isolated positions
// have their own per-market health envelope; query
// GetIsolatedHealthStatus for those.
func (k Keeper) GetHealthStatus(ctx context.Context, accountIdx uint64) (uint32, error) {
	ri, err := k.ComputeRiskInfo(ctx, accountIdx)
	if err != nil {
		return 0, err
	}
	cur := ri.CurrentRiskParameters
	if cur == nil {
		return perptypes.HealthHealthy, nil
	}
	return cur.HealthStatus(), nil
}

// GetTotalAccountValue returns TAV = collateral + sum(uPnL across CROSS
// markets) for the account. Used by public-pool share-value math
// (NAV = TAV / total_shares). Isolated positions are deliberately
// excluded, mirroring the spec's "isolated is a sub-account" rule.
func (k Keeper) GetTotalAccountValue(ctx context.Context, accountIdx uint64) (math.Int, error) {
	ri, err := k.ComputeRiskInfo(ctx, accountIdx)
	if err != nil {
		return math.ZeroInt(), err
	}
	cur := ri.CurrentRiskParameters
	if cur == nil {
		return math.ZeroInt(), nil
	}
	return cur.TotalAccountValue, nil
}

// GetAvailableCollateral returns total_account_value - initial_margin_requirement.
func (k Keeper) GetAvailableCollateral(ctx context.Context, accountIdx uint64) (math.Int, error) {
	ri, err := k.ComputeRiskInfo(ctx, accountIdx)
	if err != nil {
		return math.ZeroInt(), err
	}
	cur := ri.CurrentRiskParameters
	if cur == nil {
		return math.ZeroInt(), nil
	}
	return cur.TotalAccountValue.Sub(cur.InitialMarginRequirement), nil
}

// GetAvailableUsdcCollateral returns the amount of cross USDC collateral
// that can be safely consumed by a new isolated margin allocation
// without pushing the cross account out of HEALTHY. Mirrors
// `get_available_usdc_collateral`:
//
//   - account must currently be HEALTHY (otherwise zero — no headroom)
//   - collateral_with_funding must be non-negative (otherwise zero)
//   - take min(TAV - IMR, collateral_with_funding) clamped to zero
//
// Used by trade keeper's isolated margin auto-allocation so a maker /
// taker can be evicted (`ErrMakerInsufficientCollateral` /
// `ErrTakerInsufficientCollateral`) when a prospective fill would
// otherwise drain more cross collateral than the account currently has
// to spare.
func (k Keeper) GetAvailableUsdcCollateral(ctx context.Context, accountIdx uint64) (math.Int, error) {
	ri, err := k.ComputeRiskInfo(ctx, accountIdx)
	if err != nil {
		return math.ZeroInt(), err
	}
	cur := ri.CurrentRiskParameters
	if cur == nil {
		return math.ZeroInt(), nil
	}
	if cur.HealthStatus() != perptypes.HealthHealthy {
		return math.ZeroInt(), nil
	}
	collateral := cur.CollateralWithFunding
	if collateral.IsNegative() {
		return math.ZeroInt(), nil
	}
	avail := cur.TotalAccountValue.Sub(cur.InitialMarginRequirement)
	if avail.IsNegative() {
		return math.ZeroInt(), nil
	}
	if avail.GT(collateral) {
		return collateral, nil
	}
	return avail, nil
}

// isCrossRiskChangeValid is the cross-half of IsValidRiskChangeFrom
// (in risk_change.go). It compares the post-state cross aggregate
// against the caller-provided pre-state and defers the decision to
// classifyChange. A nil `pre` is treated as "no pre-state" and
// forces the post-state to be HEALTHY.
func (k Keeper) isCrossRiskChangeValid(ctx context.Context, accountIdx uint64, pre *types.RiskParameters) (bool, error) {
	post, err := k.ComputeRiskInfo(ctx, accountIdx)
	if err != nil {
		return false, err
	}
	postP := post.CurrentRiskParameters
	if pre == nil {
		return classifyChange(types.RiskParameters{}, *postP, true /*missingPre*/), nil
	}
	return classifyChange(*pre, *postP, false), nil
}
