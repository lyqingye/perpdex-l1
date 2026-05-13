package perp

import (
	"context"

	"cosmossdk.io/math"

	sdkerrors "cosmossdk.io/errors"

	accounttypes "github.com/perpdex/perpdex-l1/x/account/types"
	"github.com/perpdex/perpdex-l1/x/trade/types"
)

// Isolated-margin mode: every position carries its own
// `allocated_margin` pool. PnL, fees and liquidation improvement fees
// flow into / out of that pool first; only at position open / close
// boundaries is the difference reconciled with the account's cross
// collateral via `rebalanceIsolatedMargin`.
//
// Kept in a dedicated file so the dispatcher in `engine.go` can route
// per-side routing decisions in a uniform way, and a future
// `unified.go` can introduce a third margin pool without disturbing
// either the cross or the isolated leg.

// isolatedAddAllocatedMargin folds `delta` (which may be the realized
// PnL net of fees, or any other isolated-margin credit) into the
// position's `allocated_margin` and re-persists. Used by
// `applyIsolatedAccount` step 1 (cash flow) when the trade-side is
// isolated-margined.
func (e Engine) isolatedAddAllocatedMargin(ctx context.Context, res *positionChangeResult, delta math.Int) error {
	if delta.IsZero() {
		return nil
	}
	updated, err := e.accountKeeper.UpdatePosition(ctx, res.AccountIdx, res.MarketIdx, func(p *accounttypes.AccountPosition) error {
		p.AllocatedMargin = p.AllocatedMargin.Add(delta)
		return nil
	})
	if err != nil {
		return err
	}
	res.New = updated
	return nil
}

// isolatedDebit subtracts `amount` from the position's
// `allocated_margin` pool. Used by the engine's improvement-fee path
// so an isolated victim's cross account is not arbitrarily disturbed.
// Caller guarantees `amount` is non-negative.
func (e Engine) isolatedDebit(ctx context.Context, res *positionChangeResult, amount math.Int) error {
	if amount.IsZero() {
		return nil
	}
	updated, err := e.accountKeeper.UpdatePosition(ctx, res.AccountIdx, res.MarketIdx, func(p *accounttypes.AccountPosition) error {
		p.AllocatedMargin = p.AllocatedMargin.Sub(amount)
		return nil
	})
	if err != nil {
		return err
	}
	res.New = updated
	return nil
}

// applyIsolatedAccount applies one side's full post-trade effect to
// an isolated-margined account, in three sequential steps:
//
//  1. cash flow: route (realized_pnl - fee) into the position's
//     `allocated_margin` pool (`taker_collateral_delta` flows into
//     allocated_margin first for isolated positions).
//  2. maker only: debit the liquidation improvement fee from the
//     same `allocated_margin` pool so the victim's cross account is
//     not arbitrarily disturbed.
//  3. rebalance: run `rebalanceIsolatedMargin` to reconcile the
//     post-trade `allocated_margin` against the new position's IM /
//     market value (`calculate_isolated_margin_change`).
//
// Caller guarantees liqFee is non-negative and only non-zero on the
// maker side (the trade improvement fee victim). `applyAccount`
// dispatches here from `engine.go` when
// `res.Old.MarginMode == IsolatedMargin`.
func (e Engine) applyIsolatedAccount(ctx context.Context, res *positionChangeResult, fee math.Int, isMaker bool, liqFee math.Int, f Fill) error {
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

// rebalanceIsolatedMargin computes the
// `calculate_isolated_margin_change` delta for an isolated position
// and applies it: `allocated_margin += margin_delta`,
// `cross_collateral -= margin_delta`. When the delta is positive (the
// position needs MORE margin), the available cross USDC collateral is
// pre-checked via the risk keeper; insufficient headroom surfaces as
// `ErrMakerInsufficientCollateral` / `ErrTakerInsufficientCollateral`
// for the matching loop to evict the maker / stop the taker.
//
// Only called from `applyIsolatedAccount` after step 1 (cash flow)
// and step 2 (maker liquidation-fee debit) have already updated
// `res.New.AllocatedMargin`, so the routine can assume the margin
// mode is isolated and the allocated_margin reflects post-cashflow
// state.
//
// `SkipMakerRiskCheck` (and `NoRiskCheck`) skip the cross-collateral
// availability check on the maker side so the partial-liquidation
// path can still close out an isolated underwater victim. The margin
// delta itself is still applied so allocated_margin / cross collateral
// reflect the close-out's accounting cleanly.
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
	updated, err := e.accountKeeper.UpdatePosition(ctx, res.AccountIdx, res.MarketIdx, func(p *accounttypes.AccountPosition) error {
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
// `calculate_isolated_margin_change` for one side. Returns the signed
// math.Int amount that must be added to the position's
// `allocated_margin` (and removed from cross collateral) to keep the
// isolated position correctly margined after the fill:
//
//   - new position closed: -max(allocated_margin, 0)  (release the
//     remainder back to cross)
//   - side flipped: position_requirement - (allocated_margin +
//     uPnL_new)  (re-margin the new opposite-side position)
//   - same side, OI grew: max(0, oi_requirement - trade_pnl) where
//     trade_pnl = uPnL_new - uPnL_old - fee  (top up by the
//     incremental IM the fill consumed, less any PnL it generated)
//   - same side, OI shrank: -min( max(0, new_market_value -
//     target_value), max(allocated_margin, 0) ) where target_value =
//     max(ceil(old_market_value * |new| / |old|), position_requirement)
//     (release the proportional excess but never below the new
//     position's IM)
//
// `fee` is the per-side debit (in collateral units) the trade just
// paid. `res.New.AllocatedMargin` MUST already include both the
// (realized_pnl - fee) credit produced by `applyIsolatedAccount`
// step 1 AND the maker liquidation-improvement-fee debit from step
// 2, matching the ordering where the
// `taker_collateral_delta`-adjusted allocated_margin feeds into
// `calculate_isolated_margin_change`.
func (e Engine) calculateIsolatedMarginDelta(ctx context.Context, res *positionChangeResult, fee math.Int) (math.Int, error) {
	newPos := res.New
	oldPos := res.Old
	allocated := newPos.AllocatedMargin

	// case 1: new position closed → release positive allocated_margin.
	if newPos.BaseSize.IsZero() {
		if allocated.IsPositive() {
			return allocated.Neg(), nil
		}
		return math.ZeroInt(), nil
	}

	markPrice, md, err := e.marketKeeper.GetMarkPriceAndDetails(ctx, res.MarketIdx)
	if err != nil {
		return math.ZeroInt(), err
	}
	posReq := md.InitialMargin(newPos.BaseSize.Abs(), markPrice)

	// case 2: side flipped → re-margin to position_requirement at the
	// new uPnL-adjusted account state.
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
			// ceil_div(oldMV * |new|, |old|).
			num := oldMV.Mul(newAbs)
			targetValue = ceilDivPositive(num, oldAbs)
			if targetValue.LT(posReq) {
				targetValue = posReq
			}
		} else {
			// oldMV <= 0 ⇒ proportional value collapses to
			// position_requirement (`MAX(target, posReq)` with the
			// negative-target shortcut).
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

	// case 3: same side, OI grew (or stayed flat). Top up by the
	// incremental IM less any PnL the fill itself generated.
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
