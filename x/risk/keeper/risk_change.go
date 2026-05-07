package keeper

import (
	"context"

	"cosmossdk.io/collections"

	perptypes "github.com/perpdex/perpdex-l1/types"
	accounttypes "github.com/perpdex/perpdex-l1/x/account/types"
)

// risk_change.go drives the cross + isolated pre/post-state risk
// regression checks. The actual per-side decision functions live in
// cross.go (isCrossRiskChangeValid) and isolated.go
// (isIsolatedRiskChangeValid); this file just stitches them together
// across both account-wide and per-isolated-market scopes.

// IsValidRiskChange enforces the post-state vs pre-state risk
// invariants. It walks both the cross account and each isolated
// position the account holds; if either side regresses the change is
// rejected.
//
// Per-side semantics (Lighter parity):
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
func (k Keeper) IsValidRiskChange(ctx context.Context, accountIdx uint64) (bool, error) {
	if ok, err := k.isCrossRiskChangeValid(ctx, accountIdx); err != nil || !ok {
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
		if pos.Position.IsZero() || pos.MarginMode != perptypes.IsolatedMargin {
			return false
		}
		valid, err := k.isIsolatedRiskChangeValid(ctx, accountIdx, pos.MarketIndex)
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

// SnapshotPreRisk caches the pre-state RiskParameters for an account so
// IsValidRiskChange can compare after handlers run. Both the cross
// aggregate and every isolated position are snapshotted.
func (k Keeper) SnapshotPreRisk(ctx context.Context, accountIdx uint64) error {
	post, err := k.ComputeRiskInfo(ctx, accountIdx)
	if err != nil {
		return err
	}
	if post.CurrentRiskParameters != nil {
		if err := k.Cache.Set(ctx, accountIdx, *post.CurrentRiskParameters); err != nil {
			return err
		}
	}
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
		if err := k.IsolatedCache.Set(ctx, collections.Join(accountIdx, pos.MarketIndex), rp); err != nil {
			iterErr = err
			return true
		}
		return false
	}); err != nil {
		return err
	}
	return iterErr
}
