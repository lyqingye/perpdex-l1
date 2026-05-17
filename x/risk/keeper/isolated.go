package keeper

import (
	"context"

	perptypes "github.com/perpdex/perpdex-l1/types"
	accounttypes "github.com/perpdex/perpdex-l1/x/account/types"
	"github.com/perpdex/perpdex-l1/x/risk/types"
)

// isolated.go owns per-(account, market) risk computations whose
// AllocatedMargin is fenced from cross collateral. The cross +
// isolated driver IsValidRiskChangeFrom sits in risk_change.go.

// ComputeIsolatedRisk returns risk parameters for one isolated position.
func (k Keeper) ComputeIsolatedRisk(ctx context.Context, accountIdx uint64, marketIdx uint32) (types.RiskParameters, error) {
	pos, err := k.accountKeeper.GetPosition(ctx, accountIdx, marketIdx)
	if err != nil {
		return types.RiskParameters{}, err
	}
	if pos.MarginMode != perptypes.IsolatedMargin {
		return types.RiskParameters{}, accounttypes.ErrPositionNotIsolated
	}
	markPrice, md, err := k.marketKeeper.GetMarkPriceAndDetails(ctx, marketIdx)
	if err != nil {
		return types.RiskParameters{}, err
	}
	uPnL := pos.UnrealizedPnL(markPrice)
	return types.RiskParameters{
		Collateral:                   pos.AllocatedMargin,
		CollateralWithFunding:        pos.AllocatedMargin,
		TotalAccountValue:            pos.AllocatedMargin.Add(uPnL),
		InitialMarginRequirement:     pos.InitialMargin(markPrice, md),
		MaintenanceMarginRequirement: pos.MaintenanceMargin(markPrice, md),
		CloseOutMarginRequirement:    pos.CloseOutMargin(markPrice, md),
	}, nil
}

// GetIsolatedHealthStatus classifies the health of one isolated
// position. Empty positions and cross-mode requests (caller error)
// return HealthHealthy.
func (k Keeper) GetIsolatedHealthStatus(ctx context.Context, accountIdx uint64, marketIdx uint32) (uint32, error) {
	pos, err := k.accountKeeper.GetPosition(ctx, accountIdx, marketIdx)
	if err != nil {
		return 0, err
	}
	if pos.BaseSize.IsZero() || pos.MarginMode != perptypes.IsolatedMargin {
		return perptypes.HealthHealthy, nil
	}
	rp, err := k.ComputeIsolatedRisk(ctx, accountIdx, marketIdx)
	if err != nil {
		return 0, err
	}
	return rp.HealthStatus(), nil
}

// IterateIsolatedPositions walks every isolated position of the
// account, invoking fn(marketIdx, status, rp). fn returns true to
// stop. Used by liquidation/matching to evaluate isolated positions
// independently of cross health.
func (k Keeper) IterateIsolatedPositions(ctx context.Context, accountIdx uint64,
	fn func(marketIdx uint32, status uint32, rp types.RiskParameters) bool,
) error {
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
		return fn(pos.MarketIndex, rp.HealthStatus(), rp)
	}); err != nil {
		return err
	}
	return iterErr
}

// isIsolatedRiskChangeValid compares the post-state isolated params
// for one (account, market) against pre. hasPre == false forces the
// post-state to be HEALTHY.
func (k Keeper) isIsolatedRiskChangeValid(
	ctx context.Context,
	accountIdx uint64,
	marketIdx uint32,
	pre types.RiskParameters,
	hasPre bool,
) (bool, error) {
	postRP, err := k.ComputeIsolatedRisk(ctx, accountIdx, marketIdx)
	if err != nil {
		return false, err
	}
	if !hasPre {
		return classifyChange(types.RiskParameters{}, postRP, true), nil
	}
	return classifyChange(pre, postRP, false), nil
}
