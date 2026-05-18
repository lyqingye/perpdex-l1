package perp

import (
	"context"

	"cosmossdk.io/math"

	sdkerrors "cosmossdk.io/errors"

	accounttypes "github.com/perpdex/perpdex-l1/x/account/types"
	"github.com/perpdex/perpdex-l1/x/trade/types"
)

// Isolated-margin mode: every position carries its own allocated_margin
// pool. PnL / fees / improvement fees hit that pool first;
// rebalanceIsolatedMargin reconciles the diff with cross collateral
// at open / close / OI-change boundaries.

// isolatedAddAllocatedMargin folds delta (PnL net of fees, or any
// other isolated credit) into the position's allocated_margin. Routed
// through MutatePosition so the same-side invariant is enforced.
func (e Engine) isolatedAddAllocatedMargin(ctx context.Context, res *positionChangeResult, delta math.Int) error {
	if delta.IsZero() {
		return nil
	}
	updated, err := e.accountKeeper.MutatePosition(ctx, res.AccountIdx, res.MarketIdx, func(p *accounttypes.AccountPosition) error {
		p.AllocatedMargin = p.AllocatedMargin.Add(delta)
		return nil
	})
	if err != nil {
		return err
	}
	res.New = updated
	return nil
}

// isolatedDebit subtracts a non-negative amount from allocated_margin.
// Used by the improvement-fee path so an isolated victim's cross
// account is not disturbed.
func (e Engine) isolatedDebit(ctx context.Context, res *positionChangeResult, amount math.Int) error {
	if amount.IsZero() {
		return nil
	}
	updated, err := e.accountKeeper.MutatePosition(ctx, res.AccountIdx, res.MarketIdx, func(p *accounttypes.AccountPosition) error {
		p.AllocatedMargin = p.AllocatedMargin.Sub(amount)
		return nil
	})
	if err != nil {
		return err
	}
	res.New = updated
	return nil
}

// applyIsolatedAccount runs the per-side post-trade effect for an
// isolated account. Two top-level branches:
//
//  1. Closed (res.Closed == true): the position is gone (or retained as
//     leverage-only with allocated_margin = 0). We do NOT issue any
//     MutatePosition writes — the row may not exist, and even when
//     retained as leverage-only it should not carry transient
//     allocated_margin. Instead we drain the pre-close allocated_margin
//     PLUS realized_pnl PLUS improvement fee directly back to cross
//     collateral, mirroring the net effect of the pre-#91
//     "isolatedAddAllocatedMargin → rebalanceIsolatedMargin" sequence
//     in one AddCollateral call.
//
//  2. Open / Update / Flip-residual (Closed == false): standard
//     3-step flow against the still-open position row.
//
// liqFee must be non-negative and zero on takers.
func (e Engine) applyIsolatedAccount(ctx context.Context, res *positionChangeResult, fee math.Int, isMaker bool, liqFee math.Int, f Fill) error {
	if res.Closed {
		// res.New is the pre-close snapshot returned by ClosePosition;
		// AllocatedMargin reflects the pool we need to drain.
		refund := res.New.AllocatedMargin.Add(res.RealizedPnL).Sub(fee)
		if isMaker && liqFee.IsPositive() {
			refund = refund.Sub(liqFee)
		}
		if refund.IsZero() {
			return nil
		}
		return e.accountKeeper.AddCollateral(ctx, res.AccountIdx, refund)
	}

	delta := res.RealizedPnL
	if !fee.IsZero() {
		delta = delta.Sub(fee)
	}
	if !delta.IsZero() {
		if err := e.isolatedAddAllocatedMargin(ctx, res, delta); err != nil {
			return err
		}
	}
	if isMaker && liqFee.IsPositive() {
		if err := e.isolatedDebit(ctx, res, liqFee); err != nil {
			return err
		}
	}
	return e.rebalanceIsolatedMargin(ctx, res, fee, isMaker, f)
}

// rebalanceIsolatedMargin computes calculate_isolated_margin_change
// and applies it: allocated_margin += delta, cross_collateral -= delta.
// Positive delta is pre-checked against available cross USDC; a
// shortfall surfaces as Maker/Taker InsufficientCollateral so the
// matching loop can evict / stop the offending side.
//
// SkipMakerRiskCheck (and NoRiskCheck) bypass the cross-availability
// check on the maker so partial-liquidation can still close an
// underwater isolated victim; the delta is still applied so accounting
// stays consistent.
//
// The closed branch is handled in applyIsolatedAccount above; this
// function is only invoked when the position row is still open.
func (e Engine) rebalanceIsolatedMargin(ctx context.Context, res *positionChangeResult, fee math.Int, isMaker bool, f Fill) error {
	delta, err := e.calculateIsolatedMarginDelta(ctx, res, fee)
	if err != nil {
		return err
	}
	if delta.IsZero() {
		return nil
	}
	if delta.IsPositive() {
		skip := f.NoRiskCheck || (isMaker && f.SkipMakerRiskCheck)
		if !skip {
			avail, err := e.riskKeeper.GetAvailableUsdcCollateral(ctx, res.AccountIdx)
			if err != nil {
				return err
			}
			if avail.LT(delta) {
				if isMaker {
					return sdkerrors.Wrapf(types.ErrMakerInsufficientCollateral,
						"account %d available %s need %s",
						res.AccountIdx, avail.String(), delta.String())
				}
				return sdkerrors.Wrapf(types.ErrTakerInsufficientCollateral,
					"account %d available %s need %s",
					res.AccountIdx, avail.String(), delta.String())
			}
		}
	}
	updated, err := e.accountKeeper.MutatePosition(ctx, res.AccountIdx, res.MarketIdx, func(p *accounttypes.AccountPosition) error {
		p.AllocatedMargin = p.AllocatedMargin.Add(delta)
		return nil
	})
	if err != nil {
		return err
	}
	res.New = updated
	return e.accountKeeper.AddCollateral(ctx, res.AccountIdx, delta.Neg())
}

// calculateIsolatedMarginDelta is the in-Go equivalent of
// calculate_isolated_margin_change. Returns the signed amount that
// must be added to allocated_margin (and removed from cross) to keep
// the isolated position correctly margined:
//
//   - side flipped:  position_requirement - (allocated_margin + uPnL_new)
//   - same, OI grew: max(0, oi_requirement - trade_pnl)
//     trade_pnl = uPnL_new - uPnL_old - fee
//   - same, OI shrank: -min(max(0, new_mv - target_value),
//     max(allocated_margin, 0))
//     target_value = max(ceil(old_mv * |new| / |old|), position_requirement)
//
// Requires res.New.AllocatedMargin to already reflect the step 1
// PnL-fee credit AND the step 2 maker improvement-fee debit.
//
// The "closed" case is handled outside this function (in
// applyIsolatedAccount) so this helper never has to reason about a
// non-existent or leverage-only position row.
func (e Engine) calculateIsolatedMarginDelta(ctx context.Context, res *positionChangeResult, fee math.Int) (math.Int, error) {
	newPos := res.New
	oldPos := res.Old
	allocated := newPos.AllocatedMargin

	markPrice, md, err := e.marketKeeper.GetMarkPriceAndDetails(ctx, res.MarketIdx)
	if err != nil {
		return math.ZeroInt(), err
	}
	posReq := md.InitialMargin(newPos.BaseSize.Abs(), markPrice)

	// case 2: side flipped → re-margin to position_requirement at the
	// new uPnL-adjusted state.
	if res.SideFlipped {
		return posReq.Sub(allocated.Add(newPos.UnrealizedPnL(markPrice))), nil
	}

	if res.OIDelta < 0 {
		// case 4: same side, OI shrank → proportional release.
		oldMV := oldPos.MarketValue(markPrice)
		newMV := newPos.MarketValue(markPrice)

		var targetValue math.Int
		oldAbs := oldPos.BaseSize.Abs()
		newAbs := newPos.BaseSize.Abs()
		if oldMV.IsPositive() && !oldAbs.IsZero() {
			targetValue = ceilDivPositive(oldMV.Mul(newAbs), oldAbs)
			if targetValue.LT(posReq) {
				targetValue = posReq
			}
		} else {
			// oldMV <= 0: target collapses to posReq.
			targetValue = posReq
		}

		excess := newMV.Sub(targetValue)
		if excess.IsNegative() {
			excess = math.ZeroInt()
		}
		toMoveOut := allocated
		if toMoveOut.IsNegative() {
			toMoveOut = math.ZeroInt()
		}
		if excess.GT(toMoveOut) {
			excess = toMoveOut
		}
		if excess.IsZero() {
			return math.ZeroInt(), nil
		}
		return excess.Neg(), nil
	}

	// case 3: same side, OI grew (or flat). Top up by incremental IM
	// less any PnL the fill itself generated.
	oiAbs := math.NewInt(res.OIDelta).Abs()
	if oiAbs.IsZero() {
		return math.ZeroInt(), nil
	}
	oiReq := md.InitialMargin(oiAbs, markPrice)
	tradePnL := newPos.UnrealizedPnL(markPrice).Sub(oldPos.UnrealizedPnL(markPrice)).Sub(fee)
	delta := oiReq.Sub(tradePnL)
	if delta.IsNegative() {
		return math.ZeroInt(), nil
	}
	return delta, nil
}
