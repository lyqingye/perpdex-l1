package keeper

import (
	"context"
	"errors"
	"fmt"

	"cosmossdk.io/collections"

	perptypes "github.com/perpdex/perpdex-l1/types"
	accounttypes "github.com/perpdex/perpdex-l1/x/account/types"
	"github.com/perpdex/perpdex-l1/x/risk/types"
)

// isolated.go owns every per-(account, market) risk computation that
// has its own collateral envelope: the AllocatedMargin pool of an
// isolated position is fenced from cross collateral, so the health
// machine evaluates each isolated position independently. The
// per-position half of IsValidRiskChange (isIsolatedRiskChangeValid)
// also lives here because it consumes ComputeIsolatedRisk directly;
// the cross + isolated driver IsValidRiskChange itself sits in
// risk_change.go.

// ComputeIsolatedRisk returns risk parameters for one isolated position.
func (k Keeper) ComputeIsolatedRisk(ctx context.Context, accountIdx uint64, marketIdx uint32) (types.RiskParameters, error) {
	pos, err := k.accountKeeper.GetPosition(ctx, accountIdx, marketIdx)
	if err != nil {
		return types.RiskParameters{}, err
	}
	if pos.MarginMode != perptypes.IsolatedMargin {
		return types.RiskParameters{}, fmt.Errorf("position is not isolated")
	}
	mark, err := k.resolveMarkPrice(ctx, marketIdx)
	if err != nil {
		return types.RiskParameters{}, err
	}
	md, err := k.marketKeeper.GetMarketDetails(ctx, marketIdx)
	if err != nil {
		return types.RiskParameters{}, err
	}
	uPnL := pos.UnrealizedPnL(mark)
	return types.RiskParameters{
		Collateral:                   pos.AllocatedMargin,
		CollateralWithFunding:        pos.AllocatedMargin,
		TotalAccountValue:            pos.AllocatedMargin.Add(uPnL),
		InitialMarginRequirement:     pos.InitialMargin(mark, md),
		MaintenanceMarginRequirement: pos.MaintenanceMargin(mark, md),
		CloseOutMarginRequirement:    pos.CloseOutMargin(mark, md),
	}, nil
}

// GetIsolatedHealthStatus classifies the health of one isolated
// position. Returns HealthHealthy when the position is empty or in
// cross mode (the latter is a programming error from the caller).
func (k Keeper) GetIsolatedHealthStatus(ctx context.Context, accountIdx uint64, marketIdx uint32) (uint32, error) {
	pos, err := k.accountKeeper.GetPosition(ctx, accountIdx, marketIdx)
	if err != nil {
		return 0, err
	}
	if pos.Position.IsZero() || pos.MarginMode != perptypes.IsolatedMargin {
		return perptypes.HealthHealthy, nil
	}
	rp, err := k.ComputeIsolatedRisk(ctx, accountIdx, marketIdx)
	if err != nil {
		return 0, err
	}
	return rp.HealthStatus(), nil
}

// IterateIsolatedPositions walks every isolated perp position held by
// the account and invokes `fn(marketIdx, status, rp)`. `fn` may return
// `true` to stop iteration. Used by liquidation/matching to flag /
// liquidate isolated positions independently of the cross health.
func (k Keeper) IterateIsolatedPositions(ctx context.Context, accountIdx uint64,
	fn func(marketIdx uint32, status uint32, rp types.RiskParameters) bool,
) error {
	var iterErr error
	if err := k.accountKeeper.IterateAccountPositions(ctx, accountIdx, func(pos accounttypes.AccountPosition) bool {
		if pos.Position.IsZero() || pos.MarginMode != perptypes.IsolatedMargin {
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

// isIsolatedRiskChangeValid is the per-position half of
// IsValidRiskChange (in risk_change.go). It compares the post-state
// isolated parameters for one (account, market) against the
// snapshotted pre-state.
func (k Keeper) isIsolatedRiskChangeValid(ctx context.Context, accountIdx uint64, marketIdx uint32) (bool, error) {
	postRP, err := k.ComputeIsolatedRisk(ctx, accountIdx, marketIdx)
	if err != nil {
		return false, err
	}
	pre, err := k.IsolatedCache.Get(ctx, collections.Join(accountIdx, marketIdx))
	if err != nil {
		if errors.Is(err, collections.ErrNotFound) {
			return classifyChange(types.RiskParameters{}, postRP, true), nil
		}
		return false, err
	}
	return classifyChange(pre, postRP, false), nil
}
