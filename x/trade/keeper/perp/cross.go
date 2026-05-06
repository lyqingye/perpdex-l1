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
