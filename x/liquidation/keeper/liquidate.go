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
//
// There is intentionally no post-trade "top up negative collateral
// from the Insurance Fund" step. Lighter's partial-liquidation IOC
// trades at maker prices >= zero_price, and the liquidation fee is
// taxed only on the improvement above zero_price (`matching_engine.rs`
// `min(liquidation_fee, price_diff_rate)`); by construction the
// victim's collateral cannot become negative through this path. IF
// "absorption" only happens in the FULL/BANKRUPTCY tiers, where the
// IF participates as the deleverage trade counterparty (gated by
// `tryLLPAbsorb` IMR simulation), not via a silent collateral
// transfer.
func (k Keeper) Liquidate(ctx context.Context, victim uint64, marketIdx uint32, baseAmount uint64) error {
	snap, err := k.riskKeeper.GetZeroPriceSnapshot(ctx, victim, marketIdx)
	if err != nil {
		return err
	}
	pos := snap.Position
	if pos.Size_.IsZero() {
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
	absVictim := pos.Size_.Abs()
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
	zeroPrice := snap.ZeroPrice
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
// Risk-check policy (Lighter `InternalDeleverageTx` parity, with
// perpdex defense-in-depth on the deleverager side):
//
//   - Bankrupt (maker) post-trade `IsValidRiskChangeFrom` is ALWAYS run.
//   - LLP / Insurance Fund deleveragers (PUBLIC_POOL / INSURANCE_FUND
//     account types, or the canonical InsuranceFundOperator account)
//     SKIP the post-trade risk check on the deleverager side — they
//     are willing absorbers by mandate, mirroring Lighter where
//     `is_valid_risk_change` is asserted on bankrupt but NOT on the
//     IF deleverager.
//   - User-ADL deleveragers KEEP their post-trade risk check
//     (perpdex-stricter than Lighter, which substitutes a
//     collateral-only guard).
//
// Pre-trade collateral asserts (`is_*_has_enough_cross_collateral`):
//
//   - User-ADL deleverager: asserted (Lighter parity for
//     `is_deleverager_has_enough_cross_collateral`).
//   - LLP / IF deleverager: not asserted — the LLP IMR gate in
//     `tryLLPAbsorb` already vets pool capacity; the IF is an
//     unconditional absorber.
//   - Bankrupt: NOT asserted. Lighter's
//     `is_bankrupt_has_enough_cross_collateral` predicate relies on
//     `zero_price` zeroing the bankrupt's collateral by construction;
//     perpdex's `GetPositionZeroPrice` uses the TAV/MMR ratio
//     formulation uniformly across PARTIAL/FULL/BANKRUPTCY, which
//     produces extreme prices for deeply-bankrupt accounts and would
//     reject every legitimate close-out under a strict assert.
//     perpdex's design is "residual debt is allowed to persist on
//     the victim ledger" (see post-trade comment below); enforcing
//     the assert here would block the EndBlocker waterfall instead
//     of advancing it. Re-enabling requires aligning the zero-price
//     formula with Lighter's bankrupt branch first.
//
// The deleverager assert mirrors Lighter's `is_delta_negative`
// short-circuit: when the side's predicted realized PnL is
// non-negative (it gains collateral from the trade) the check is
// trivially satisfied.
func (k Keeper) Deleverage(ctx context.Context, victim uint64, marketIdx uint32, deleverager uint64, baseAmount uint64) error {
	snap, err := k.riskKeeper.GetZeroPriceSnapshot(ctx, victim, marketIdx)
	if err != nil {
		return err
	}
	pos := snap.Position
	if pos.Size_.IsZero() {
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
	absVictim := pos.Size_.Abs()
	if math.NewIntFromUint64(baseAmount).GT(absVictim) {
		return types.ErrInvalidADLCounterparty.Wrapf(
			"base_amount=%d exceeds victim position size %s", baseAmount, absVictim.String(),
		)
	}
	zeroPrice := snap.ZeroPrice

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
		if dPos.Size_.IsZero() {
			return types.ErrInvalidADLCounterparty.Wrap("deleverager has no position")
		}
		if dPos.Size_.IsNegative() == pos.Size_.IsNegative() {
			return types.ErrInvalidADLCounterparty.Wrap("deleverager is on the same side as victim")
		}
		absDeleverager := dPos.Size_.Abs()
		if math.NewIntFromUint64(baseAmount).GT(absDeleverager) {
			return types.ErrInvalidADLCounterparty.Wrapf(
				"base_amount=%d exceeds deleverager position size %s",
				baseAmount, absDeleverager.String(),
			)
		}
	}

	takerIsAsk := pos.Size_.IsNegative()

	// Pre-trade collateral assert on the deleverager side only
	// (Lighter `is_deleverager_has_enough_cross_collateral` parity).
	// The bankrupt side is not asserted — see Deleverage docstring.
	// IF / Pool deleveragers are absorbers by mandate and bypass the
	// check; the LLP IMR gate in `tryLLPAbsorb` already vets pool
	// capacity. Settles pending funding before reading collateral so
	// the comparison is funding-aware (matches `Engine.Apply` step 1).
	if !isInsuranceFund && !isPoolDeleverager {
		if err := k.preCheckCollateral(
			ctx, deleverager, marketIdx, baseAmount, zeroPrice,
			true /*isTakerSide*/, takerIsAsk, "deleverager",
		); err != nil {
			return err
		}
	}

	if err := k.tradeKeeper.ApplyPerpsMatching(ctx, tradekeeper.PerpFill{
		MakerAccountIndex: victim,
		TakerAccountIndex: deleverager,
		MarketIndex:       marketIdx,
		Price:             zeroPrice,
		BaseAmount:        baseAmount,
		IsTakerAsk:        takerIsAsk,
		NoFee:             true,
		// Lighter `internal_deleverage.rs` parity (and defense-in-
		// depth on perpdex's side):
		//
		//   * Bankrupt (maker in our convention) is ALWAYS subject
		//     to `IsValidRiskChangeFrom` — the trade is supposed to
		//     mechanically improve their TAV/MMR ratio, and the
		//     check guards against pathological pricing/funding
		//     interactions that would silently regress them.
		//   * Insurance Fund / Public Pool deleveragers are exempt
		//     from the post-trade risk regression check — they are
		//     willing absorbers, mirroring Lighter where
		//     `is_valid_risk_change` is asserted on bankrupt but
		//     NOT on the deleverager when it is the IF. perpdex
		//     keeps this asymmetry but also retains the user-ADL
		//     deleverager check (defense-in-depth) instead of
		//     swapping it for Lighter's collateral-only guard.
		SkipTakerRiskCheck: isInsuranceFund || isPoolDeleverager,
	}); err != nil {
		return err
	}
	// Intentionally no post-trade collateral top-up: Lighter's
	// `InternalDeleverageTx` settles bankrupt and deleverager at
	// `zero_quote`, which by construction zeroes out the bankrupt's
	// proportional collateral. Any residual negative collateral
	// (rounding, funding accruals between zero-price computation and
	// trade application) is allowed to persist as an account-level
	// debt on the victim ledger — exactly mirroring Lighter, which
	// has no equivalent silent IF top-up.
	return nil
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

// preCheckCollateral implements Lighter's
// `is_deleverager_has_enough_cross_collateral` guard for the
// deleverager side of a Deleverage / autoADL trade.
//
// The bankrupt side is intentionally NOT routed through this helper;
// see `Deleverage`'s docstring for the rationale (perpdex's uniform
// TAV/MMR-ratio zero-price formula produces extreme prices for deeply
// bankrupt accounts which would fail the strict assert universally,
// blocking the EndBlocker waterfall instead of advancing it).
//
// Behaviour:
//
//  1. Settle pending funding on the (account, market) position. This
//     is idempotent (Engine.Apply step 1 does the same) and ensures
//     the post-funding `EntryQuote` feeds into the predicted PnL — so
//     the comparison is funding-aware, just like Lighter reading
//     `available_cross_collateral` from a `risk_info` that already
//     incorporates `usdc_collateral_with_funding`.
//  2. Compute the predicted realized PnL via the same pure `ApplyFill`
//     used by `Engine.applyPositionChange` so the assert and the
//     engine cannot drift on sign / scaling.
//  3. Short-circuit when the predicted RealizedPnL is non-negative —
//     in perpdex's frame `applyCrossAccount` adds RealizedPnL
//     directly to `Collateral`, so a non-negative RealizedPnL means
//     the side's collateral does not shrink and no cushion is
//     required.
//  4. Otherwise, compare the side's available collateral against
//     `|RealizedPnL|`. Cross uses `account.Collateral`; isolated uses
//     `pos.AllocatedMargin` — mirroring Lighter's per-account split
//     between `cross_risk_parameters` and `current_risk_parameters`.
//
// On rejection returns `types.ErrInsufficientCollateral`; the
// EndBlocker callers (`tryLLPAbsorb` / `autoADL`) treat it as a
// graceful "skip this candidate" signal so the waterfall can advance
// to the next ADL counterparty without aborting the whole block.
//
// Sign conventions match `Engine.applyPositionChange`:
//
//	makerSign := +1 if takerIsAsk else -1
//	takerSign := -makerSign
func (k Keeper) preCheckCollateral(
	ctx context.Context,
	accountIdx uint64,
	marketIdx uint32,
	base uint64,
	zeroPrice uint32,
	isTakerSide bool,
	takerIsAsk bool,
	label string,
) error {
	if err := k.fundingKeeper.SettlePositionFunding(ctx, accountIdx, marketIdx); err != nil {
		return err
	}
	pos, err := k.accountKeeper.GetPosition(ctx, accountIdx, marketIdx)
	if err != nil {
		return err
	}
	var sign int64
	switch {
	case isTakerSide && takerIsAsk:
		sign = -1
	case isTakerSide && !takerIsAsk:
		sign = +1
	case !isTakerSide && takerIsAsk:
		sign = +1
	default:
		sign = -1
	}
	delta := math.NewIntFromUint64(base).MulRaw(sign)
	fill := pos.ApplyFill(delta, zeroPrice)
	realized := fill.RealizedPnL
	if !realized.IsNegative() {
		// `applyCrossAccount` adds RealizedPnL directly to
		// Collateral, so a non-negative value means the side's
		// collateral does not shrink — no cushion required.
		// Mirrors Lighter's `is_delta_negative` short-circuit.
		return nil
	}
	required := realized.Abs()
	var available math.Int
	switch pos.MarginMode {
	case perptypes.IsolatedMargin:
		available = pos.AllocatedMargin
	default:
		a, err := k.accountKeeper.GetAccount(ctx, accountIdx)
		if err != nil {
			return err
		}
		available = a.Collateral
	}
	if available.LT(required) {
		return types.ErrInsufficientCollateral.Wrapf(
			"%s account=%d market=%d available=%s required=%s",
			label, accountIdx, marketIdx, available.String(), required.String(),
		)
	}
	return nil
}
