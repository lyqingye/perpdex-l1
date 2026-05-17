package keeper

import (
	"context"

	errorsmod "cosmossdk.io/errors"
	"cosmossdk.io/math"

	perptypes "github.com/perpdex/perpdex-l1/types"
	accounttypes "github.com/perpdex/perpdex-l1/x/account/types"
	"github.com/perpdex/perpdex-l1/x/risk/types"
)

// cross.go owns the cross-margin half of the risk keeper: the
// ComputeCrossRisk aggregation, the pure-read accessors built on top
// of it, and the cross-side of IsValidRiskChangeFrom. The cross +
// isolated driver and SnapshotRisk live in risk_change.go.

// ComputeCrossRisk aggregates the account's CROSS positions into a
// RiskParameters value. Isolated positions are excluded — their
// AllocatedMargin/uPnL are fenced from cross collateral, and mixing
// them into TAV without their IM/MM/CM would let an isolated profit
// silently dodge cross liquidation. Isolated risk goes through
// ComputeIsolatedRisk.
func (k Keeper) ComputeCrossRisk(ctx context.Context, accountIdx uint64) (types.RiskParameters, error) {
	a, err := k.accountKeeper.GetAccount(ctx, accountIdx)
	if err != nil {
		return types.RiskParameters{}, err
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
		// Isolated positions are evaluated independently.
		if pos.MarginMode == perptypes.IsolatedMargin {
			return false
		}
		// Fail-closed on the mark read: silently zeroing the
		// contribution on an oracle outage would hide risk
		// regressions.
		markPrice, md, err := k.marketKeeper.GetMarkPriceAndDetails(ctx, pos.MarketIndex)
		if err != nil {
			iterErr = errorsmod.Wrapf(err, "account=%d", accountIdx)
			return true
		}
		imSum = imSum.Add(pos.InitialMargin(markPrice, md))
		mmSum = mmSum.Add(pos.MaintenanceMargin(markPrice, md))
		cmSum = cmSum.Add(pos.CloseOutMargin(markPrice, md))
		totalCross = totalCross.Add(pos.UnrealizedPnL(markPrice))
		return false
	}); err != nil {
		return types.RiskParameters{}, err
	}
	if iterErr != nil {
		return types.RiskParameters{}, iterErr
	}

	cross.TotalAccountValue = totalCross
	cross.InitialMarginRequirement = imSum
	cross.MaintenanceMarginRequirement = mmSum
	cross.CloseOutMarginRequirement = cmSum
	return cross, nil
}

// GetHealthStatus returns the CROSS health. Isolated positions have
// their own per-market envelope; use GetIsolatedHealthStatus for them.
func (k Keeper) GetHealthStatus(ctx context.Context, accountIdx uint64) (uint32, error) {
	rp, err := k.ComputeCrossRisk(ctx, accountIdx)
	if err != nil {
		return 0, err
	}
	return rp.HealthStatus(), nil
}

// GetTotalAccountValue returns TAV = collateral + sum(uPnL) over
// CROSS markets. Used by public-pool NAV math; isolated positions are
// excluded (isolated is a sub-account by spec).
func (k Keeper) GetTotalAccountValue(ctx context.Context, accountIdx uint64) (math.Int, error) {
	rp, err := k.ComputeCrossRisk(ctx, accountIdx)
	if err != nil {
		return math.ZeroInt(), err
	}
	return rp.TotalAccountValue, nil
}

// GetAvailableCollateral returns total_account_value - initial_margin_requirement.
func (k Keeper) GetAvailableCollateral(ctx context.Context, accountIdx uint64) (math.Int, error) {
	rp, err := k.ComputeCrossRisk(ctx, accountIdx)
	if err != nil {
		return math.ZeroInt(), err
	}
	return rp.TotalAccountValue.Sub(rp.InitialMarginRequirement), nil
}

// GetAvailableUsdcCollateral returns the cross USDC headroom safe to
// fund a new isolated margin allocation without dropping the cross
// account out of HEALTHY:
//
//   - account must be HEALTHY (else 0)
//   - collateral_with_funding must be non-negative (else 0)
//   - take min(TAV - IMR, collateral_with_funding) clamped to 0
//
// Used by trade keeper's isolated auto-allocation to evict makers /
// stop takers via Maker/Taker InsufficientCollateral.
func (k Keeper) GetAvailableUsdcCollateral(ctx context.Context, accountIdx uint64) (math.Int, error) {
	rp, err := k.ComputeCrossRisk(ctx, accountIdx)
	if err != nil {
		return math.ZeroInt(), err
	}
	if rp.HealthStatus() != perptypes.HealthHealthy {
		return math.ZeroInt(), nil
	}
	collateral := rp.CollateralWithFunding
	if collateral.IsNegative() {
		return math.ZeroInt(), nil
	}
	avail := rp.TotalAccountValue.Sub(rp.InitialMarginRequirement)
	if avail.IsNegative() {
		return math.ZeroInt(), nil
	}
	if avail.GT(collateral) {
		return collateral, nil
	}
	return avail, nil
}

// isCrossRiskChangeValid compares the post-state cross aggregate
// against pre and delegates to classifyChange. A nil pre is treated
// as "no pre-state" and forces the post-state to be HEALTHY.
func (k Keeper) isCrossRiskChangeValid(ctx context.Context, accountIdx uint64, pre *types.RiskParameters) (bool, error) {
	post, err := k.ComputeCrossRisk(ctx, accountIdx)
	if err != nil {
		return false, err
	}
	if pre == nil {
		return classifyChange(types.RiskParameters{}, post, true /*missingPre*/), nil
	}
	return classifyChange(*pre, post, false), nil
}
