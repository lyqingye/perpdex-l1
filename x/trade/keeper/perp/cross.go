package perp

import (
	"context"

	"cosmossdk.io/math"

	accounttypes "github.com/perpdex/perpdex-l1/x/account/types"
)

// Cross-margin mode: PnL / fees / improvement fees flow through the
// account's cross USDC collateral. No per-position pool to sync, so
// these helpers are thin wrappers over the account keeper.

// crossAddCollateral credits (or debits, when delta < 0) the account's
// cross USDC pool.
func (e Engine) crossAddCollateral(ctx context.Context, accountIdx uint64, delta math.Int) error {
	return e.accountKeeper.AddCollateral(ctx, accountIdx, delta)
}

// crossDebit subtracts a non-negative amount from cross collateral.
// Used by the improvement-fee path when the victim is cross-margined.
func (e Engine) crossDebit(ctx context.Context, accountIdx uint64, amount math.Int) error {
	if amount.IsZero() {
		return nil
	}
	return e.accountKeeper.AddCollateral(ctx, accountIdx, amount.Neg())
}

// applyCrossAccount routes (realized_pnl - fee) into cross collateral
// and, on the maker side only, debits the liquidation improvement fee
// from the same pool. liqFee must be non-negative and zero on takers.
func (e Engine) applyCrossAccount(ctx context.Context, res *accounttypes.FillApplyResult, fee math.Int, isMaker bool, liqFee math.Int) error {
	accountIdx := res.Old.AccountIndex
	delta := res.RealizedPnL
	if !fee.IsZero() {
		delta = delta.Sub(fee)
	}
	if !delta.IsZero() {
		if err := e.crossAddCollateral(ctx, accountIdx, delta); err != nil {
			return err
		}
	}
	if isMaker && liqFee.IsPositive() {
		return e.crossDebit(ctx, accountIdx, liqFee)
	}
	return nil
}
