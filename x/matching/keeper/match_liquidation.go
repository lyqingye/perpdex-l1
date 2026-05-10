package keeper

import (
	"context"
	"errors"

	sdk "github.com/cosmos/cosmos-sdk/types"

	perptypes "github.com/perpdex/perpdex-l1/types"
	"github.com/perpdex/perpdex-l1/x/matching/types"
	orderbooktypes "github.com/perpdex/perpdex-l1/x/orderbook/types"
	tradekeeper "github.com/perpdex/perpdex-l1/x/trade/keeper"
)

// MatchLiquidationOrder is the system-only entry point used by the
// liquidation keeper to drive a partial-liquidation close-out through
// the public orderbook (Lighter parity with `InternalLiquidatePositionTx`
// + `LIQUIDATION_ORDER + IOC + reduce_only` flow).
//
// The synthetic taker is owned by the victim. It is constructed in-
// memory only — never persisted via `OpenOrder`, never indexed against
// the account-open / client-id maps, and never counted against the
// per-account open-order cap. IOC residue is silently discarded by
// returning the partial `filled` count.
//
// The caller is expected to have already cancelled every resting
// order owned by the victim (via `CancelAllOpenOrdersForAccount`)
// before invoking this entry point so a victim's own bids cannot
// front-run the close-out fill (matching Lighter's
// `InternalCancelAllOrdersTx → InternalLiquidatePositionTx` ordering).
//
// Side direction is derived from the victim's current position:
// long victim → sell to close (IsAsk=true), short victim → buy to
// close (IsAsk=false). The order is reduce-only, so the matching loop
// will cap each fill against the victim's residual position size and
// can never accidentally flip the account to the opposite side.
//
// `liquidationFeeBps` is the market's configured liquidation fee
// (passed through the trade engine's improvement-over-zero-price
// formula); `liquidationFeeRecipient` is typically the Insurance Fund
// operator account but is left explicit so future LLP-targeted
// recipients can be wired without touching this surface.
func (k Keeper) MatchLiquidationOrder(
	ctx context.Context,
	victim uint64,
	marketIdx uint32,
	zeroPrice uint32,
	baseAmount uint64,
	liquidationFeeBps uint32,
	liquidationFeeRecipient uint64,
) (uint64, error) {
	if baseAmount == 0 {
		return 0, types.ErrInvalidOrder.Wrap("liquidation base_amount must be > 0")
	}
	if zeroPrice == 0 {
		return 0, types.ErrInvalidOrder.Wrap("liquidation zero price must be > 0")
	}
	pos, err := k.accountKeeper.GetPosition(ctx, victim, marketIdx)
	if err != nil {
		return 0, err
	}
	if pos.Size_.IsZero() {
		return 0, types.ErrInvalidOrder.Wrapf("victim=%d has no position in market=%d", victim, marketIdx)
	}
	// Long victim closes via SELL (taker ask); short victim closes
	// via BUY (taker bid). Mirrors x/liquidation/keeper/liquidate.go
	// `takerIsAsk := pos.Size_.IsNegative()` semantics — except
	// here the victim is the taker, so the sign flips.
	isAsk := pos.Size_.IsPositive()
	// Cap requested base by victim's |position| so the synthetic
	// reduce-only IOC cannot ask for more than the close-out size.
	abs := pos.Size_.Abs().Uint64()
	if baseAmount > abs {
		baseAmount = abs
	}

	params, err := k.Params.Get(ctx)
	if err != nil {
		return 0, err
	}
	maxFills := params.MaxFillsPerMsg
	if maxFills == 0 {
		maxFills = 64
	}

	now := sdk.UnwrapSDKContext(ctx).BlockTime().UnixMilli()
	taker := orderbooktypes.Order{
		// OrderIndex / Nonce intentionally left zero: the synthetic
		// taker is never persisted, never indexed, and never compared
		// against book entries via OrderIndex. The matching kernel only
		// reads MarketIndex / OwnerAccountIndex / IsAsk / OrderType /
		// Price / RemainingBaseAmount / ReduceOnly / Expiry from the
		// taker.
		OwnerAccountIndex:   victim,
		MarketIndex:         marketIdx,
		IsAsk:               isAsk,
		OrderType:           perptypes.LiquidationOrder,
		TimeInForce:         perptypes.IOC,
		ReduceOnly:          true,
		Price:               zeroPrice,
		InitialBaseAmount:   baseAmount,
		RemainingBaseAmount: baseAmount,
		CreatedAt:           now,
		Status:              perptypes.OrderStatusOpen,
	}
	return k.matchLiquidation(ctx, &taker, maxFills, zeroPrice, liquidationFeeBps, liquidationFeeRecipient)
}

// matchLiquidation runs the matching loop for a system-issued
// `LIQUIDATION_ORDER + IOC + reduce_only` taker. The synthetic taker
// is owned by the victim and is NEVER persisted to the orderbook —
// IOC residue is silently discarded by `MatchLiquidationOrder`.
//
// Invariants the caller (MatchLiquidationOrder) is expected to have
// established before reaching this loop:
//
//   - taker.OrderType == LiquidationOrder
//   - taker.TimeInForce == IOC
//   - taker.ReduceOnly == true
//   - taker.OwnerAccountIndex == victim
//   - taker.Price == zeroPrice (zero-price floor; the price-reachable
//     check inside nextMaker guarantees fills only happen at maker
//     prices not worse than zeroPrice, matching Lighter's "fill at or
//     better than zero price" guarantee)
//
// After every successful fill the loop re-evaluates the victim's
// health (cross or per-market isolated, matching the liquidation
// keeper's `victimHealthForPosition` rule) and breaks early when the
// account is no longer in PARTIAL/FULL liquidation. This is Lighter's
// `is_not_in_liquidation_and_is_liquidation_order` short-circuit: a
// liquidation order keeps consuming the book only as long as the
// victim still needs deleveraging.
//
// A recoverable taker-side regression (errTakerRejected; the victim's
// post-fill state somehow regresses against trade-engine invariants)
// is treated as a graceful stop: prior writeCache fills are kept,
// the loop terminates, and the IOC residue is discarded by the
// caller.
func (k Keeper) matchLiquidation(
	ctx context.Context,
	taker *orderbooktypes.Order,
	maxFills uint32,
	zeroPrice uint32,
	liquidationFeeBps uint32,
	liquidationFeeRecipient uint64,
) (uint64, error) {
	now := sdk.UnwrapSDKContext(ctx).BlockTime().UnixMilli()
	market, err := k.marketKeeper.GetMarket(ctx, taker.MarketIndex)
	if err != nil {
		return 0, err
	}
	// Liquidation orders only exist for perps markets in Lighter's
	// design. Spot markets have no notion of liquidation.
	if market.MarketType != perptypes.MarketTypePerps {
		return 0, types.ErrInvalidOrder.Wrapf(
			"liquidation order requires perps market (got type=%d)", market.MarketType,
		)
	}

	var totalFilled uint64
	var fills uint32
	for taker.RemainingBaseAmount > 0 && fills < maxFills {
		maker, ok, err := k.nextMaker(ctx, taker, true /* isPerp */, now)
		if err != nil {
			return totalFilled, err
		}
		if !ok {
			break
		}

		base, ok, err := k.matchSize(ctx, taker, maker, true)
		if err != nil {
			return totalFilled, err
		}
		if !ok {
			break
		}

		committed, err := k.applyLiquidationFill(
			ctx, taker, maker, base,
			zeroPrice, liquidationFeeBps, liquidationFeeRecipient,
		)
		if errors.Is(err, errTakerRejected) {
			// Recoverable taker rejection on the victim: prior
			// fills are retained, residue is dropped by the IOC
			// caller. Mirrors the legacy fillStepTakerAbort
			// behavior of the old matchLiquidationLoop.
			return totalFilled, nil
		}
		if err != nil {
			return totalFilled, err
		}
		if !committed {
			// Maker recoverable error: bad maker has been evicted
			// on the outer ctx. Re-peek without advancing the
			// taker residue or fill counter.
			continue
		}

		taker.RemainingBaseAmount -= base
		totalFilled += base
		fills++
		k.emitOrderFill(ctx, taker.MarketIndex, maker.Price, base)

		// Health short-circuit: stop consuming the book the moment
		// the victim is no longer in liquidation. Intentionally
		// placed AFTER the fill is accounted for so the just-
		// applied write commits even if the post-fill account
		// becomes healthy on the same iteration.
		stillNeeded, err := k.needsLiquidation(ctx, taker.OwnerAccountIndex, taker.MarketIndex)
		if err != nil {
			return totalFilled, err
		}
		if !stillNeeded {
			break
		}
	}
	return totalFilled, nil
}

// applyLiquidationFill builds the PerpFill record specific to a
// liquidation IOC iteration and dispatches to the matching-core
// apply helper. The differences vs the user path live entirely in
// this builder:
//
//   - TakerFee / MakerFee = 0 (the only fee charged is the
//     liquidation improvement fee computed inside the trade engine).
//
//   - ZeroPrice / LiquidationFeeBps / LiquidationFeeRecipient flow
//     into the trade engine so any improvement above the zero-price
//     floor is captured and routed to the configured recipient
//     (Insurance Fund / LLP).
//
//   - Risk checks: both maker and taker are validated post-trade,
//     mirroring Lighter `matching_engine.rs:1801,1843` where a
//     LIQUIDATION_ORDER + IOC fill runs `is_valid_risk_change` on
//     both sides. Recoverable rejections are wrapped into
//     errMakerRejected / errTakerRejected by `applyPerpFill`; the
//     enclosing `matchLiquidation` loop evicts a bad maker and
//     gracefully stops on a bad taker (preserving prior fills).
//
//     The taker side (victim) almost always passes by construction
//     (filling at >= zero price strictly improves TAV/MMR), but
//     leaving the check in place catches pathological pricing /
//     funding interactions and removes the previous "trust placement
//     vetting" assumption on the maker side, since the maker's
//     account state may have changed between order placement and
//     this fill (other fills, funding accruals).
func (k Keeper) applyLiquidationFill(
	ctx context.Context,
	taker *orderbooktypes.Order,
	maker orderbooktypes.OrderBookEntry,
	base uint64,
	zeroPrice uint32,
	liquidationFeeBps uint32,
	liquidationFeeRecipient uint64,
) (bool, error) {
	return k.applyPerpFill(ctx, maker, tradekeeper.PerpFill{
		MakerAccountIndex:       maker.OwnerAccountIndex,
		TakerAccountIndex:       taker.OwnerAccountIndex,
		MarketIndex:             taker.MarketIndex,
		Price:                   maker.Price,
		BaseAmount:              base,
		IsTakerAsk:              taker.IsAsk,
		TakerFee:                0,
		MakerFee:                0,
		ZeroPrice:               zeroPrice,
		LiquidationFeeBps:       liquidationFeeBps,
		LiquidationFeeRecipient: liquidationFeeRecipient,
	})
}

// needsLiquidation reports whether a victim is still subject to
// liquidation in the targeted market. The classification mirrors
// x/liquidation's `victimHealthForPosition`: cross-mode positions
// consult the cross account health; isolated positions consult the
// per-market isolated health, since each isolated position is its
// own risk envelope.
//
// Used exclusively by `matchLiquidation` to implement the Lighter
// `is_not_in_liquidation_and_is_liquidation_order` short-circuit.
// It is intentionally NOT called from `matchOrder` so the user-path
// matching loop pays no per-fill risk-keeper read.
//
// The accepted health-status set mirrors Lighter's `is_in_liquidation`
// predicate (risk_info.rs:362):
//
//	is_in_liquidation = (TALT.sign == -1) ∨ (TALT.abs < MMR)
//
// In perpdex's single-asset USDC mode (no non-USDC margined assets)
// `total_account_liquidation_threshold` collapses onto
// `total_account_value`, so the predicate reduces to `TAV < MMR`,
// which spans PARTIAL_LIQUIDATION ∪ FULL_LIQUIDATION ∪ BANKRUPTCY.
// BANKRUPTCY is included for spec parity even though entry to the
// IOC loop requires PARTIAL and fills only improve TAV — funding
// accruals between fills could in theory push an account from
// PARTIAL through FULL into BANKRUPTCY mid-loop.
func (k Keeper) needsLiquidation(ctx context.Context, victim uint64, marketIdx uint32) (bool, error) {
	pos, err := k.accountKeeper.GetPosition(ctx, victim, marketIdx)
	if err != nil {
		return false, err
	}
	var s uint32
	if pos.MarginMode == perptypes.IsolatedMargin {
		s, err = k.riskKeeper.GetIsolatedHealthStatus(ctx, victim, marketIdx)
	} else {
		s, err = k.riskKeeper.GetHealthStatus(ctx, victim)
	}
	if err != nil {
		return false, err
	}
	return s == perptypes.HealthPartialLiquidation ||
		s == perptypes.HealthFullLiquidation ||
		s == perptypes.HealthBankruptcy, nil
}
