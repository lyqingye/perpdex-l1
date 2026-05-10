package keeper

import (
	"context"

	"github.com/perpdex/perpdex-l1/x/account/types"
	risktypes "github.com/perpdex/perpdex-l1/x/risk/types"
)

// rejectPoolAccount refuses to let the generic account Msg handlers operate on
// public pool / insurance fund accounts. LP collateral must flow exclusively
// through MintShares / BurnShares / StrategyTransfer / liquidation paths so
// the share bookkeeping (TotalShares/OperatorShares/users' PublicPoolShares)
// stays consistent with the pool's NAV.
func (k Keeper) rejectPoolAccount(ctx context.Context, idx uint64) error {
	a, err := k.GetAccount(ctx, idx)
	if err != nil {
		return err
	}
	if a.IsPoolType() {
		return types.ErrPoolGenericMsg.Wrapf("account %d is a pool account", idx)
	}
	return nil
}

// settleAllPositionFunding settles pending funding for every non-zero
// perp position held by `accountIdx`. Called before
// Withdraw/Transfer/UpdateMargin so the subsequent risk check sees
// the post-funding EntryQuote and not a stale snapshot. Walks only
// persisted position rows.
func (k Keeper) settleAllPositionFunding(ctx context.Context, accountIdx uint64) error {
	var settleErr error
	err := k.IterateAccountPositions(ctx, accountIdx, func(pos types.AccountPosition) bool {
		if pos.Size_.IsZero() {
			return false
		}
		if err := k.fundingKeeper.SettlePositionFunding(ctx, accountIdx, pos.MarketIndex); err != nil {
			settleErr = err
			return true
		}
		return false
	})
	if err != nil {
		return err
	}
	return settleErr
}

// requireRiskOKFrom enforces a post-state risk check against the
// caller-provided pre-state snapshot. The risk keeper is wired at
// app construction and is always non-nil at runtime; missing wiring
// is a programming error and will panic, which is the desired
// fail-fast behaviour.
func (k Keeper) requireRiskOKFrom(ctx context.Context, accountIdx uint64, pre risktypes.PreRiskSnapshot) error {
	ok, err := k.riskKeeper.IsValidRiskChangeFrom(ctx, accountIdx, pre)
	if err != nil {
		return err
	}
	if !ok {
		return types.ErrRiskRegression
	}
	return nil
}

// snapshotPreRisk captures the account's pre-state risk envelope by
// value so a later requireRiskOKFrom call can compare deltas instead
// of demanding a strictly-healthy post state. The returned value is
// scoped to the current handler — passing it across handlers (or
// persisting it) defeats the regression check.
func (k Keeper) snapshotPreRisk(ctx context.Context, accountIdx uint64) (risktypes.PreRiskSnapshot, error) {
	return k.riskKeeper.SnapshotRisk(ctx, accountIdx)
}
