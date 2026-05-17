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
// Risk-check policy mirrors Lighter's `internal_deleverage` apply-time
// pattern (`bankrupt_account_valid_risk_change` +
// `is_*_has_enough_cross_collateral`) but is expressed in perpdex's
// risk envelope:
//
//   - Bankrupt risk regression: enforced by the trade engine's
//     `IsValidRiskChangeFrom` on the maker side (we DO NOT pass
//     `SkipMakerRiskCheck`). Same role as Lighter's
//     `bankrupt_account_valid_risk_change`.
//   - Bankrupt has-enough-collateral: NOT asserted. A strict assert
//     here would lean on `zero_price` zeroing the bankrupt's
//     collateral by construction; perpdex's `GetPositionZeroPrice`
//     uses the TAV/MMR ratio formulation uniformly across
//     PARTIAL/FULL/BANKRUPTCY, which produces extreme prices for
//     deeply-bankrupt accounts and would reject every legitimate
//     close-out. perpdex's design is "residual debt is allowed to
//     persist on the victim ledger" (see post-trade comment below);
//     enforcing the assert would block the EndBlocker waterfall
//     instead of advancing it. Re-enabling requires aligning the
//     zero-price formula with a dedicated bankrupt branch first.
//   - User-deleverager has-enough-collateral: enforced AFTER
//     `ApplyPerpsMatching` returns, by re-reading the deleverager's
//     cross risk envelope and asserting `post.TAV >= post.IMR`
//     (HEALTHY). The check is positioned post-fill on purpose: the
//     trade engine has already mutated the cosmos-sdk store branch,
//     so a fresh `ComputeCrossRisk` reads the real post-state for
//     free, while a pre-fill simulator would have to re-derive the
//     same risk numbers. If the assert fails the branch is rolled
//     back via the returned error — fail-late but state-safe.
//
//     This is the perpdex analogue of Lighter's apply-time
//     `is_deleverager_has_enough_cross_collateral`. Lighter's
//     `available_pre >= |margin_delta|` is algebraically equivalent
//     to `post.TAV >= post.IMR` after substituting the wider
//     margin_delta = ΔIMR - ΔTAV that perpdex's `MsgDeleverage`
//     requires (because `size` is a user-controlled msg field and
//     the settle price is the victim's zero price, NOT
//     `ZeroPriceMid`, so the price-by-construction invariant alone
//     does not save the deleverager when the victim is in deep
//     BANKRUPTCY and its zero price has crossed the mark).
//   - User-deleverager risk regression
//     (`IsValidRiskChangeFrom`): NOT run on the taker side
//     (`SkipTakerRiskCheck=true`). The post-fill HEALTHY check
//     above already implies non-regression (HEALTHY post-state
//     accepted unconditionally by `IsValidRiskChangeFrom` per
//     `classifyChange`), and skipping the duplicate
//     `ComputeCrossRisk` call inside the trade engine keeps the
//     hot path lean.
//   - IF / Pool deleverager: not asserted on either side. The IF
//     is an unconditional absorber; LLP capacity is vetted upstream
//     by `tryLLPAbsorb`'s IMR gate.
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

	if err := k.tradeKeeper.ApplyPerpsMatching(ctx, tradekeeper.PerpFill{
		MakerAccountIndex: victim,
		TakerAccountIndex: deleverager,
		MarketIndex:       marketIdx,
		Price:             zeroPrice,
		BaseAmount:        baseAmount,
		IsTakerAsk:        takerIsAsk,
		NoFee:             true,
		// Bankrupt (maker) keeps trade engine's IsValidRiskChangeFrom
		// — that is the bankrupt-side equivalent of Lighter's
		// `bankrupt_account_valid_risk_change` apply-time assert.
		// All deleverager flavours (user / IF / Pool) skip the trade
		// engine taker risk regression: IF and Pool are absorbers by
		// mandate; user-deleveragers are governed by the explicit
		// post-fill HEALTHY check below, which is the perpdex
		// equivalent of Lighter's apply-time
		// `is_deleverager_has_enough_cross_collateral` (size upper
		// bound expressed via post-state cushion) — `MsgDeleverage`'s
		// `size` is a msg input so this assertion is what bounds
		// it. Skipping the trade engine's taker check here avoids a
		// redundant `ComputeCrossRisk` call (the post-state read
		// below reuses the state ApplyPerpsMatching just wrote).
		SkipTakerRiskCheck: true,
	}); err != nil {
		return err
	}

	// Post-fill cushion assertion for user-deleveragers, mirroring
	// Lighter's apply-time `is_deleverager_has_enough_cross_collateral`
	// in spirit but expressed in perpdex's post-state HEALTHY form
	// (algebraically equivalent: post.TAV >= post.IMR is the same
	// invariant Lighter encodes as `available_pre >= |margin_delta|`
	// after substituting perpdex's wider margin_delta = ΔIMR - ΔTAV).
	//
	// Why post-state read instead of pre-fill simulate: the trade
	// engine's `ApplyPerpsMatching` already wrote the post-state in
	// the current cosmos-sdk store branch. A `ComputeCrossRisk` here
	// reads the real post-state for free; a pre-fill simulator would
	// have to re-derive the same numbers and is strictly redundant.
	// If the assert fails, returning a non-nil error rolls back the
	// branch — same observable outcome as fail-fast, but without the
	// duplicated risk computation on the success path (which is the
	// hot path).
	//
	// IF / Pool deleveragers are NOT subject to this assert. They are
	// absorbers of last resort by mandate and may legitimately fall
	// out of HEALTHY when soaking up a victim's residual exposure;
	// LLP capacity is governed upstream in `tryLLPAbsorb` instead.
	//
	// The autoADL path does NOT call this function and is not subject
	// to this assert: there the settle price is `ZeroPriceMid(victim,
	// candidate)` and `size` is bounded by the protocol-internal
	// queue, so deleverager non-regression is guaranteed by Lighter-
	// style price-by-construction + size-bounded invariants. The
	// exposure here is unique to `MsgDeleverage`, where `size` is a
	// user-controlled msg field and the settle price is the victim's
	// zero price (NOT the mid), so the price-by-construction
	// invariant alone does not save the deleverager when the victim
	// is in deep BANKRUPTCY and its zero price has crossed the mark.
	if !isInsuranceFund && !isPoolDeleverager {
		postCross, err := k.riskKeeper.ComputeCrossRisk(ctx, deleverager)
		if err != nil {
			return err
		}
		if postCross.HealthStatus() != perptypes.HealthHealthy {
			return types.ErrInvalidADLCounterparty.Wrapf(
				"deleverage at price=%d size=%d would push deleverager %d out of HEALTHY (post.TAV=%s post.IMR=%s)",
				zeroPrice, baseAmount, deleverager,
				postCross.TotalAccountValue.String(),
				postCross.InitialMarginRequirement.String(),
			)
		}
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

