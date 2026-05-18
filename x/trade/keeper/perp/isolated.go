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
//
// All AllocatedMargin writes go through the cohesive
// accountKeeper.AdjustAllocatedMargin (issue #91) — the trade engine
// never issues an RMW closure of its own. Closed positions short-
// circuit at the top of applyIsolatedAccount and route refunds
// straight to cross collateral via AddCollateral.

// applyIsolatedAccount runs the per-side post-trade effect for an
// isolated account. Two top-level branches:
//
//  1. Closed (res.Closed == true): the position is gone (or retained as
//     leverage-only with allocated_margin = 0). We do NOT issue any
//     AdjustAllocatedMargin writes — the row may not exist, and even
//     when retained as leverage-only it should not carry transient
//     allocated_margin. Instead we drain the pre-close
//     allocated_margin PLUS realized_pnl PLUS improvement fee
//     directly back to cross collateral, mirroring the net effect of
//     the pre-#91 "add → debit → rebalance" sequence in one
//     AddCollateral call.
//
//  2. Open / Update / Flip-residual (Closed == false): standard
//     3-step flow:
//       (a) PnL/fee credit                 → AdjustAllocatedMargin(+(pnl-fee))
//       (b) Improvement-fee debit (maker)  → AdjustAllocatedMargin(-liqFee)
//       (c) Position-requirement rebalance → rebalanceIsolatedMargin
//
// liqFee must be non-negative and zero on takers.
func (e Engine) applyIsolatedAccount(ctx context.Context, res *accounttypes.FillApplyResult, fee math.Int, isMaker bool, liqFee math.Int, f Fill) error {
	accountIdx := res.Old.AccountIndex
	marketIdx := res.Old.MarketIndex

	if res.Closed {
		// res.New is the pre-close snapshot returned by ApplyFill's
		// close branch; AllocatedMargin reflects the pool we need to
		// drain.
		refund := res.New.AllocatedMargin.Add(res.RealizedPnL).Sub(fee)
		if isMaker && liqFee.IsPositive() {
			refund = refund.Sub(liqFee)
		}
		if refund.IsZero() {
			return nil
		}
		return e.accountKeeper.AddCollateral(ctx, accountIdx, refund)
	}

	// Step 1: fold (PnL - fee) into allocated_margin.
	delta := res.RealizedPnL
	if !fee.IsZero() {
		delta = delta.Sub(fee)
	}
	if !delta.IsZero() {
		updated, err := e.accountKeeper.AdjustAllocatedMargin(ctx, accountIdx, marketIdx, delta)
		if err != nil {
			return err
		}
		res.New = updated
	}

	// Step 2: maker improvement-fee debit from allocated_margin.
	if isMaker && liqFee.IsPositive() {
		updated, err := e.accountKeeper.AdjustAllocatedMargin(ctx, accountIdx, marketIdx, liqFee.Neg())
		if err != nil {
			return err
		}
		res.New = updated
	}

	// Step 3: position-requirement rebalance (delta := posReq -
	// allocated_after_step12).
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
func (e Engine) rebalanceIsolatedMargin(ctx context.Context, res *accounttypes.FillApplyResult, fee math.Int, isMaker bool, f Fill) error {
	accountIdx := res.Old.AccountIndex
	marketIdx := res.Old.MarketIndex

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
			avail, err := e.riskKeeper.GetAvailableUsdcCollateral(ctx, accountIdx)
			if err != nil {
				return err
			}
			if avail.LT(delta) {
				if isMaker {
					return sdkerrors.Wrapf(types.ErrMakerInsufficientCollateral,
						"account %d available %s need %s",
						accountIdx, avail.String(), delta.String())
				}
				return sdkerrors.Wrapf(types.ErrTakerInsufficientCollateral,
					"account %d available %s need %s",
					accountIdx, avail.String(), delta.String())
			}
		}
	}
	updated, err := e.accountKeeper.AdjustAllocatedMargin(ctx, accountIdx, marketIdx, delta)
	if err != nil {
		return err
	}
	res.New = updated
	return e.accountKeeper.AddCollateral(ctx, accountIdx, delta.Neg())
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
func (e Engine) calculateIsolatedMarginDelta(ctx context.Context, res *accounttypes.FillApplyResult, fee math.Int) (math.Int, error) {
	newPos := res.New
	oldPos := res.Old
	allocated := newPos.AllocatedMargin

	markPrice, md, err := e.marketKeeper.GetMarkPriceAndDetails(ctx, res.Old.MarketIndex)
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
