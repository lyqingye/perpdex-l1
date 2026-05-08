package keeper

import (
	"context"
	"strconv"

	"cosmossdk.io/math"

	sdk "github.com/cosmos/cosmos-sdk/types"

	perptypes "github.com/perpdex/perpdex-l1/types"
	accounttypes "github.com/perpdex/perpdex-l1/x/account/types"
	"github.com/perpdex/perpdex-l1/x/liquidation/types"
	tradekeeper "github.com/perpdex/perpdex-l1/x/trade/keeper"
)

// Liquidate is the keeper entry point for MsgLiquidate. It implements
// the Lighter partial-liquidation procedure:
//
//  1. Verify the victim is in PARTIAL_LIQUIDATION. FULL/BANKRUPTCY are
//     out of scope here — those tiers are handled by EndBlocker via
//     the LLP take-over → ADL waterfall, mirroring Lighter's
//     `InternalDeleverageTx` separation from `InternalLiquidatePositionTx`.
//  2. Cancel every open order owned by the victim. A victim's resting
//     bids could otherwise front-run the close-out fill — Lighter
//     parity with the standalone `InternalCancelAllOrdersTx` that
//     precedes a partial liquidation.
//  3. Compute the position's mark-based zero price (TAV/MMR ratio
//     invariant) — the worst price the victim is allowed to receive.
//  4. Submit a synthetic `LIQUIDATION_ORDER + IOC + reduce_only` to
//     the matching keeper on behalf of the victim. The order trades
//     against the open book at maker prices that improve on the zero
//     price; any improvement is taxed at `market.LiquidationFee` and
//     routed to the LLP / Insurance Fund. The matching loop also
//     short-circuits the moment the victim is no longer in
//     liquidation.
//  5. Top up any residual negative collateral from the Insurance Fund
//     so a single MsgLiquidate cannot leave residual debt on the chain.
func (k Keeper) Liquidate(ctx context.Context, victim uint64, marketIdx uint32, baseAmount uint64) error {
	pos, err := k.accountKeeper.GetPosition(ctx, victim, marketIdx)
	if err != nil {
		return err
	}
	if pos.Position.IsZero() {
		return types.ErrNotLiquidatable.Wrap("victim has no position")
	}
	// Determine the relevant health (cross account vs isolated
	// position) based on the victim's margin mode for this market.
	status, err := k.victimHealthForPosition(ctx, victim, marketIdx, pos)
	if err != nil {
		return err
	}
	// MsgLiquidate is intentionally restricted to PARTIAL: FULL and
	// BANKRUPTCY are deleverage / IF / LLP territory and are driven
	// by EndBlocker (see abci.go). A keeper bot that sees a
	// FULL/BANKRUPTCY account should not race the EndBlocker by
	// issuing MsgLiquidate.
	if status != perptypes.HealthPartialLiquidation {
		return types.ErrNotLiquidatable.Wrapf(
			"victim status=%d not partial; FULL/BANKRUPTCY routes via EndBlocker LLP→ADL",
			status,
		)
	}
	if baseAmount == 0 {
		return types.ErrInvalidParams.Wrap("base_amount must be > 0")
	}
	// A partial-liquidation Msg that passes in more base than the
	// victim's remaining size would otherwise close the position *and*
	// flip it to the opposite side, stealing collateral from the
	// victim. Cap here (symmetrical to Deleverage).
	absVictim := pos.Position.Abs()
	if math.NewIntFromUint64(baseAmount).GT(absVictim) {
		return types.ErrInvalidParams.Wrapf(
			"base_amount=%d exceeds victim position size %s", baseAmount, absVictim.String(),
		)
	}
	// Cancel-all orders BEFORE the IOC close-out, mirroring lighter's
	// `InternalCancelAllOrdersTx → InternalLiquidatePositionTx`
	// ordering. We tolerate a missing matching keeper only as a
	// graceful fall-through for stub-driven tests.
	if k.matchingKeeper != nil {
		if _, err := k.matchingKeeper.CancelAllOpenOrdersForAccount(ctx, victim); err != nil {
			return err
		}
	}
	zeroPrice, err := k.riskKeeper.GetPositionZeroPrice(ctx, victim, marketIdx)
	if err != nil {
		return err
	}
	market, err := k.marketKeeper.GetMarket(ctx, marketIdx)
	if err != nil {
		return err
	}

	// Drive the close-out through the public order book. The matching
	// keeper synthesises a victim-owned LIQUIDATION_ORDER + IOC +
	// reduce_only and consumes opposite makers at prices that improve
	// on the zero price. The synthetic taker is never persisted; IOC
	// residue is silently discarded.
	var filled uint64
	if k.matchingKeeper != nil {
		filled, err = k.matchingKeeper.MatchLiquidationOrder(
			ctx, victim, marketIdx, zeroPrice, baseAmount,
			market.LiquidationFee, perptypes.InsuranceFundOperatorAccountIdx,
		)
		if err != nil {
			return err
		}
	}

	if err := k.absorbNegativeCollateral(ctx, victim); err != nil {
		return err
	}
	sdk.UnwrapSDKContext(ctx).EventManager().EmitEvent(sdk.NewEvent(
		types.EventTypeLiquidate,
		sdk.NewAttribute(types.AttributeKeyVictim, strconv.FormatUint(victim, 10)),
		sdk.NewAttribute(types.AttributeKeyMarketIndex, strconv.FormatUint(uint64(marketIdx), 10)),
		sdk.NewAttribute(types.AttributeKeyBaseAmount, strconv.FormatUint(filled, 10)),
		sdk.NewAttribute(types.AttributeKeyZeroPrice, strconv.FormatUint(uint64(zeroPrice), 10)),
	))
	return nil
}


// Deleverage is the keeper entry for MsgDeleverage and the engine path
// used by EndBlocker for both LLP takeover and user-side ADL fills.
//
// For LLP / Insurance Fund deleveragers (account_type == PUBLIC_POOL or
// INSURANCE_FUND, or the canonical InsuranceFundOperator account) the
// fill bypasses post-trade risk checks because the pool's share-
// holders explicitly opted into absorbing residual loss. User-ADL
// counterparties go through the standard checks since their close-out
// strictly improves their account.
func (k Keeper) Deleverage(ctx context.Context, victim uint64, marketIdx uint32, deleverager uint64, baseAmount uint64) error {
	pos, err := k.accountKeeper.GetPosition(ctx, victim, marketIdx)
	if err != nil {
		return err
	}
	if pos.Position.IsZero() {
		return types.ErrNotLiquidatable.Wrap("victim has no position")
	}
	status, err := k.victimHealthForPosition(ctx, victim, marketIdx, pos)
	if err != nil {
		return err
	}
	if status != perptypes.HealthFullLiquidation && status != perptypes.HealthBankruptcy {
		return types.ErrNotBankrupt.Wrapf("status=%d", status)
	}
	if deleverager == victim {
		return types.ErrInvalidADLCounterparty.Wrap("deleverager equals victim")
	}
	if baseAmount == 0 {
		return types.ErrInvalidParams.Wrap("base_amount must be > 0")
	}
	absVictim := pos.Position.Abs()
	if math.NewIntFromUint64(baseAmount).GT(absVictim) {
		return types.ErrInvalidADLCounterparty.Wrapf(
			"base_amount=%d exceeds victim position size %s", baseAmount, absVictim.String(),
		)
	}
	zeroPrice, err := k.riskKeeper.GetPositionZeroPrice(ctx, victim, marketIdx)
	if err != nil {
		return err
	}

	dAcc, err := k.accountKeeper.GetAccount(ctx, deleverager)
	if err != nil {
		return err
	}
	isPoolDeleverager := dAcc.IsPoolType()
	if isPoolDeleverager {
		if err := accounttypes.EnsureActive(dAcc.PublicPoolInfo); err != nil {
			return accounttypes.ErrPoolFrozen.Wrapf(
				"deleverager pool %d is not ACTIVE", deleverager,
			)
		}
	}

	isInsuranceFund := deleverager == perptypes.InsuranceFundOperatorAccountIdx
	if !isInsuranceFund && !isPoolDeleverager {
		// User ADL path: enforce opposite-side and size bound on the
		// counterparty. Same sign means we'd be growing one side's
		// position — never valid for ADL.
		dPos, err := k.accountKeeper.GetPosition(ctx, deleverager, marketIdx)
		if err != nil {
			return err
		}
		if dPos.Position.IsZero() {
			return types.ErrInvalidADLCounterparty.Wrap("deleverager has no position")
		}
		if dPos.Position.IsNegative() == pos.Position.IsNegative() {
			return types.ErrInvalidADLCounterparty.Wrap("deleverager is on the same side as victim")
		}
		absDeleverager := dPos.Position.Abs()
		if math.NewIntFromUint64(baseAmount).GT(absDeleverager) {
			return types.ErrInvalidADLCounterparty.Wrapf(
				"base_amount=%d exceeds deleverager position size %s",
				baseAmount, absDeleverager.String(),
			)
		}
	}

	takerIsAsk := pos.Position.IsNegative()
	if err := k.tradeKeeper.ApplyPerpsMatching(ctx, tradekeeper.PerpFill{
		MakerAccountIndex: victim,
		TakerAccountIndex: deleverager,
		MarketIndex:       marketIdx,
		Price:             zeroPrice,
		BaseAmount:        baseAmount,
		IsTakerAsk:        takerIsAsk,
		NoFee:             true,
		// Insurance fund / Public Pool absorb residual risk
		// regardless of their own post-state health. User-ADL
		// counterparties go through the standard taker risk check
		// (their position is closing toward zero so it should always
		// pass); the maker (victim) side is skipped because the
		// close-out is mechanically improving by construction.
		NoRiskCheck:        isInsuranceFund || isPoolDeleverager,
		SkipMakerRiskCheck: !isInsuranceFund && !isPoolDeleverager,
	}); err != nil {
		return err
	}
	return k.absorbNegativeCollateral(ctx, victim)
}

// absorbNegativeCollateral tops up a victim's negative cross-collateral
// from the Insurance Fund operator account. Used by every close-out
// path so a single MsgLiquidate cannot leave residual debt on the
// chain.
func (k Keeper) absorbNegativeCollateral(ctx context.Context, victim uint64) error {
	a, err := k.accountKeeper.GetAccount(ctx, victim)
	if err != nil {
		return err
	}
	if !a.Collateral.IsNegative() {
		return nil
	}
	if err := k.accountKeeper.AddCollateral(ctx, perptypes.InsuranceFundOperatorAccountIdx, a.Collateral); err != nil {
		return err
	}
	return k.accountKeeper.AddCollateral(ctx, victim, a.Collateral.Neg())
}

// victimHealthForPosition picks the right health-status getter for the
// targeted (victim, market) pair. Cross positions read the cross
// account health; isolated positions read the per-market isolated
// health, since each isolated position is a distinct risk envelope.
func (k Keeper) victimHealthForPosition(
	ctx context.Context, victim uint64, marketIdx uint32, pos accounttypes.AccountPosition,
) (uint32, error) {
	if pos.MarginMode == perptypes.IsolatedMargin {
		return k.riskKeeper.GetIsolatedHealthStatus(ctx, victim, marketIdx)
	}
	return k.riskKeeper.GetHealthStatus(ctx, victim)
}
