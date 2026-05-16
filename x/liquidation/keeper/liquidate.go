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

// Canonical `source` attribute values for `EventTypeDeleverage`.
const (
	DeleverageSourceMsg     = "msg"
	DeleverageSourceLLP     = "llp"
	DeleverageSourceAutoADL = "auto_adl"
)

// DeleverageOption tags the entry point on the emitted
// `EventTypeDeleverage`.
type DeleverageOption func(*deleverageOpts)

type deleverageOpts struct {
	source string
}

// WithDeleverageSource sets the `source` attribute. Pass one of the
// `DeleverageSource*` constants.
func WithDeleverageSource(source string) DeleverageOption {
	return func(o *deleverageOpts) { o.source = source }
}

// Liquidate is the keeper entry point for MsgLiquidate. It implements
// the partial-liquidation procedure:
//
//  1. Verify the victim is in PARTIAL_LIQUIDATION. FULL/BANKRUPTCY are
//     out of scope here — those tiers are handled by EndBlocker via
//     the LLP take-over → ADL waterfall, which is a distinct
//     deleverage path from the partial-liquidation tx.
//  2. Cancel every open order owned by the victim. A victim's resting
//     bids could otherwise front-run the close-out fill — the
//     cancel-all step always precedes the partial-liquidation IOC.
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
// from the Insurance Fund" step. The partial-liquidation IOC trades
// at maker prices >= zero_price, and the per-trade liquidation fee
// is taxed only on the improvement above zero_price (capped at
// `min(market.LiquidationFee, price_diff_rate)`); by construction
// the victim's collateral cannot become negative through this path.
// IF "absorption" only happens in the FULL/BANKRUPTCY tiers, where
// the IF participates as the deleverage trade counterparty (gated
// by `tryLLPAbsorb` IMR simulation), not via a silent collateral
// transfer.
func (k Keeper) Liquidate(ctx context.Context, victim uint64, marketIdx uint32, baseAmount uint64) error {
	snap, err := k.riskKeeper.GetZeroPriceSnapshot(ctx, victim, marketIdx)
	if err != nil {
		return err
	}
	pos := snap.Position
	if pos.BaseSize.IsZero() {
		return types.ErrNotLiquidatable.Wrap("victim has no position")
	}
	status, err := k.healthEnvelopeFor(ctx, victim, marketIdx, pos.MarginMode)
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
	absVictim := pos.BaseSize.Abs()
	if math.NewIntFromUint64(baseAmount).GT(absVictim) {
		return types.ErrInvalidParams.Wrapf(
			"base_amount=%d exceeds victim position size %s", baseAmount, absVictim.String(),
		)
	}
	// Cancel-all orders BEFORE the IOC close-out so a victim's
	// resting bids cannot front-run the close-out fill. We tolerate
	// a missing matching keeper only as a graceful fall-through for
	// stub-driven tests.
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
// Risk-check policy (deleverage trade settles bankrupt+deleverager
// at the victim's zero price, with perpdex defense-in-depth on the
// deleverager side):
//
//   - Bankrupt (maker) post-trade `IsValidRiskChangeFrom` is ALWAYS run.
//   - LLP / Insurance Fund deleveragers (PUBLIC_POOL / INSURANCE_FUND
//     account types, or the canonical InsuranceFundOperator account)
//     SKIP the post-trade risk check on the deleverager side — they
//     are willing absorbers by mandate, so the post-trade risk
//     regression assert is asserted on the bankrupt but NOT on the
//     pool deleverager.
//   - User-ADL deleveragers KEEP their post-trade risk check
//     (defense-in-depth, stricter than a collateral-only guard).
//
// Pre-trade collateral asserts:
//
//   - User-ADL deleverager: asserted via `preCheckCollateral`
//     ("deleverager has enough cross collateral for the predicted
//     realized loss").
//   - LLP / IF deleverager: not asserted — the LLP IMR gate in
//     `tryLLPAbsorb` already vets pool capacity; the IF is an
//     unconditional absorber.
//   - Bankrupt: NOT asserted. A strict "bankrupt has enough cross
//     collateral" assert would rely on `zero_price` zeroing the
//     bankrupt's collateral by construction; perpdex's
//     `GetPositionZeroPrice` uses the TAV/MMR ratio formulation
//     uniformly across PARTIAL/FULL/BANKRUPTCY, which produces
//     extreme prices for deeply-bankrupt accounts and would reject
//     every legitimate close-out under a strict assert. perpdex's
//     design is "residual debt is allowed to persist on the victim
//     ledger" (see post-trade comment below); enforcing the assert
//     here would block the EndBlocker waterfall instead of
//     advancing it. Re-enabling requires aligning the zero-price
//     formula with a dedicated bankrupt branch first.
//
// The deleverager assert short-circuits when the side's predicted
// realized PnL is non-negative (it gains collateral from the trade)
// — the check is trivially satisfied in that case.
//
// `opts` only carries the entry-point label emitted on the resulting
// `EventTypeDeleverage`. Default is `source="msg"`; the LLP absorb
// path passes `WithDeleverageSource(DeleverageSourceLLP)`. autoADL
// does NOT route through here — it issues its own ADL trade at
// `ZeroPriceMid(victimZP, candZP)` directly against the trade engine.
func (k Keeper) Deleverage(
	ctx context.Context,
	victim uint64, marketIdx uint32, deleverager uint64, baseAmount uint64,
	opts ...DeleverageOption,
) error {
	cfg := deleverageOpts{source: DeleverageSourceMsg}
	for _, o := range opts {
		o(&cfg)
	}

	snap, err := k.riskKeeper.GetZeroPriceSnapshot(ctx, victim, marketIdx)
	if err != nil {
		return err
	}
	pos := snap.Position
	if pos.BaseSize.IsZero() {
		return types.ErrNotLiquidatable.Wrap("victim has no position")
	}
	status, err := k.healthEnvelopeFor(ctx, victim, marketIdx, pos.MarginMode)
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
	absVictim := pos.BaseSize.Abs()
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
		// MsgDeleverage lets a user pick the counterparty, so the
		// same-side / size-cap guard is required here. autoADL does
		// not route through this function.
		dPos, err := k.accountKeeper.GetPosition(ctx, deleverager, marketIdx)
		if err != nil {
			return err
		}
		if dPos.BaseSize.IsZero() {
			return types.ErrInvalidADLCounterparty.Wrap("deleverager has no position")
		}
		if dPos.IsLong() == pos.IsLong() {
			return types.ErrInvalidADLCounterparty.Wrap("deleverager is on the same side as victim")
		}
		absDeleverager := dPos.BaseSize.Abs()
		if math.NewIntFromUint64(baseAmount).GT(absDeleverager) {
			return types.ErrInvalidADLCounterparty.Wrapf(
				"base_amount=%d exceeds deleverager position size %s",
				baseAmount, absDeleverager.String(),
			)
		}
	}

	takerIsAsk := pos.OpeningIsAsk()

	// Pre-trade collateral assert on the deleverager side only — the
	// bankrupt side is not asserted (see docstring). IF / Pool
	// deleveragers are absorbers by mandate and skip the check.
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
		// Bankrupt (maker) is always risk-checked. IF / Pool
		// deleveragers are absorbers by mandate and skip the
		// post-trade taker risk regression; user deleveragers keep
		// it as defense-in-depth.
		SkipTakerRiskCheck: isInsuranceFund || isPoolDeleverager,
	}); err != nil {
		return err
	}
	// No post-trade IF top-up: residual negative collateral persists
	// on the victim ledger as account-level debt.
	sdk.UnwrapSDKContext(ctx).EventManager().EmitEvent(sdk.NewEvent(
		types.EventTypeDeleverage,
		sdk.NewAttribute(types.AttributeKeyVictim, strconv.FormatUint(victim, 10)),
		sdk.NewAttribute(types.AttributeKeyDeleverager, strconv.FormatUint(deleverager, 10)),
		sdk.NewAttribute(types.AttributeKeyMarketIndex, strconv.FormatUint(uint64(marketIdx), 10)),
		sdk.NewAttribute(types.AttributeKeyBaseAmount, strconv.FormatUint(baseAmount, 10)),
		sdk.NewAttribute(types.AttributeKeyPrice, strconv.FormatUint(uint64(zeroPrice), 10)),
		sdk.NewAttribute(types.AttributeKeySource, cfg.source),
	))
	return nil
}

// healthEnvelopeFor picks the right health-status getter for the
// targeted (account, market) pair. Cross positions read the cross
// account health; isolated positions read the per-market isolated
// health, since each isolated position is a distinct risk envelope.
//
// Used by both Liquidate/Deleverage (MsgLiquidate / MsgDeleverage entry
// points) and processAccount (EndBlocker) so the cross-vs-isolated
// routing rule is defined exactly once.
func (k Keeper) healthEnvelopeFor(
	ctx context.Context, accIdx uint64, marketIdx uint32, marginMode uint32,
) (uint32, error) {
	if marginMode == perptypes.IsolatedMargin {
		return k.riskKeeper.GetIsolatedHealthStatus(ctx, accIdx, marketIdx)
	}
	return k.riskKeeper.GetHealthStatus(ctx, accIdx)
}

// preCheckCollateral implements the "deleverager has enough cross
// collateral to absorb the predicted realized loss" guard for the
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
//  1. Settle pending funding on the (account, market) position so the
//     post-funding `EntryQuote` feeds into the predicted PnL — the
//     comparison is funding-aware. Idempotent: `Engine.Apply` step 1
//     does the same.
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
//     `pos.AllocatedMargin` — mirroring the per-account split between
//     the cross aggregate (ComputeCrossRisk) and the per-position
//     isolated envelope (ComputeIsolatedRisk).
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
		// collateral does not shrink — no cushion required, so the
		// guard short-circuits as trivially satisfied.
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
