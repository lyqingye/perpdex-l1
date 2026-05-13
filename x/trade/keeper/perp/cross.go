package perp

import (
	"context"

	"cosmossdk.io/math"
)

// Cross-margin mode: PnL / fees / liquidation improvements all flow
// directly through the account's cross USDC collateral. There is no
// per-position allocated_margin to keep in sync, which makes these
// helpers thin wrappers around the account keeper's cross collateral
// mutator.
//
// Kept in a dedicated file so the surface mirrors `isolated.go` and a
// future `unified.go` can drop in a third symmetric set of mode-
// specific helpers without disturbing the cross / isolated paths.

// crossAddCollateral credits (or debits, when delta is negative) the
// account's cross USDC collateral pool. Used by the engine's
// post-trade financial routing for cross-margined positions.
func (e Engine) crossAddCollateral(ctx context.Context, accountIdx uint64, delta math.Int) error {
	return e.accountKeeper.AddCollateral(ctx, accountIdx, delta)
}

// crossDebit subtracts `amount` from the account's cross USDC
// collateral pool. The caller is responsible for ensuring the amount
// is non-negative; the engine's improvement-fee path uses this to
// charge the victim when the victim is cross-margined.
func (e Engine) crossDebit(ctx context.Context, accountIdx uint64, amount math.Int) error {
	if amount.IsZero() {
		return nil
	}
	return e.accountKeeper.AddCollateral(ctx, accountIdx, amount.Neg())
}

// applyCrossAccount applies one side's full post-trade effect to a
// cross-margined account: route (realized_pnl - fee) into cross
// collateral, then (maker only) debit the liquidation improvement fee
// from the same cross pool. Cross has no per-position margin pool to
// rebalance, so the function is intentionally short — `applyAccount`
// dispatches here from `engine.go` when `res.Old.MarginMode != IsolatedMargin`.
//
// Two-stage cash flow:
//   - PnL.Sub(fee) → cross collateral (via crossAddCollateral)
//   - liqFee debited from cross collateral (via crossDebit)
//
// Caller guarantees liqFee is non-negative and only non-zero on the
// maker side (the trade improvement fee victim).
func (e Engine) applyCrossAccount(ctx context.Context, res *positionChangeResult, fee math.Int, isMaker bool, liqFee math.Int) error {
	delta := res.RealizedPnL
	if !fee.IsZero() {
		delta = delta.Sub(fee)
	}
	if !delta.IsZero() {
		if err := e.crossAddCollateral(ctx, res.AccountIdx, delta); err != nil {
			return err
		}
	}
	if isMaker && liqFee.IsPositive() {
		return e.crossDebit(ctx, res.AccountIdx, liqFee)
	}
	return nil
}
