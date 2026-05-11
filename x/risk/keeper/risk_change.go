package keeper

import (
	"context"

	perptypes "github.com/perpdex/perpdex-l1/types"
	accounttypes "github.com/perpdex/perpdex-l1/x/account/types"
	"github.com/perpdex/perpdex-l1/x/risk/types"
)

// risk_change.go drives the cross + isolated pre/post-state risk
// regression checks. The actual per-side decision functions live in
// cross.go (isCrossRiskChangeValid) and isolated.go
// (isIsolatedRiskChangeValid); this file just stitches them together
// across both account-wide and per-isolated-market scopes.
//
// Pre-state lives in a function-local `types.PreRiskSnapshot` value
// threaded through by the caller (engine.Apply, account msg-server).
// There is no chain-level KV cache: a pre-state that outlived its
// handler would silently leak into the next Msg's regression check.

// IsValidRiskChangeFrom enforces the post-state vs pre-state risk
// invariants. It walks both the cross account and each isolated
// position the account holds; if either side regresses the change is
// rejected.
//
// Per-side semantics:
//
//   - HEALTHY post-state is accepted unconditionally.
//   - PRE_LIQUIDATION pre-state: post must remain at most PRE,
//     post.MMR <= pre.MMR (no new exposure on the same mark), AND
//     TAV/MMR ratio cannot decrease. This implements the spec's
//     "do not increase the size of any position and do not decrease
//     the account value to maintenance margin requirement ratio"
//     rule. Mark prices are constant across pre/post inside the same
//     block, so the MMR comparison is equivalent to a per-position
//     |size| comparison.
//   - Otherwise (PARTIAL/FULL/BANKRUPTCY pre-state): post.class <=
//     pre.class AND TAV/IM ratio cannot decrease. Routine user trades
//     in these states are rejected up-front by the matching layer; the
//     check here is the safety net for liquidation-initiated fills.
//
// `pre` MUST be the value returned by SnapshotRisk at the start of
// the same handler. A zero-value snapshot is treated as "no pre-state"
// and forces the post-state to be HEALTHY (fail-closed rule).
func (k Keeper) IsValidRiskChangeFrom(ctx context.Context, accountIdx uint64, pre types.PreRiskSnapshot) (bool, error) {
	if ok, err := k.isCrossRiskChangeValid(ctx, accountIdx, pre.Cross); err != nil || !ok {
		return ok, err
	}
	// Walk each isolated position and require it to satisfy the same
	// invariants. We do not error when a pre-snapshot is missing for
	// an isolated position: the position may have just been opened
	// in this fill, so we fall back to "post must be HEALTHY".
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

// SnapshotRisk computes the pre-state RiskParameters for an account
// and returns them by value. Both the cross aggregate and every
// isolated position are captured; an isolated market that the account
// does not currently hold a non-zero position in is not recorded so
// IsValidRiskChangeFrom falls back to "post must be HEALTHY" if the
// position is opened during the handler.
func (k Keeper) SnapshotRisk(ctx context.Context, accountIdx uint64) (types.PreRiskSnapshot, error) {
	snap := types.PreRiskSnapshot{}
	cross, err := k.ComputeRiskInfo(ctx, accountIdx)
	if err != nil {
		return types.PreRiskSnapshot{}, err
	}
	if cross.CurrentRiskParameters != nil {
		rp := *cross.CurrentRiskParameters
		snap.Cross = &rp
	}
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
