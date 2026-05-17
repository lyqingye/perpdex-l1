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

// Deleverage is the keeper entry for MsgDeleverage and the LLP-takeover
// path called from EndBlocker. autoADL does NOT route through here —
// it issues its own ADL trade at `ZeroPriceMid(victimZP, candZP)`
// directly against the trade engine.
//
// Risk checks:
//
//   - Bankrupt (maker) post-trade health regression is enforced by
//     the trade engine's IsValidRiskChangeFrom (we do NOT set
//     SkipMakerRiskCheck).
//   - Bankrupt collateral sufficiency is NOT asserted. The
//     unified TAV/MMR-ratio zero-price formula produces extreme
//     prices for deeply-bankrupt accounts; a strict assert would
//     reject every legitimate close-out. Residual negative
//     collateral is allowed to persist on the victim ledger.
//   - User-deleverager must remain HEALTHY after the fill. This
//     bounds the user-supplied `baseAmount`, which is otherwise
//     unconstrained. The check runs after ApplyPerpsMatching so
//     it reads real post-state instead of re-simulating; a
//     non-nil return rolls back the store branch.
//   - IF / Pool deleveragers skip the post-fill check; they are
//     absorbers by design (LLP capacity is vetted upstream by
//     tryLLPAbsorb's IMR gate).
//
// SkipTakerRiskCheck=true on the trade engine call lets the post-fill
// HEALTHY check below own deleverager-side enforcement and avoids a
// redundant ComputeCrossRisk inside the engine.
//
// `opts` only carries the entry-point label emitted on the resulting
// EventTypeDeleverage. Default is source="msg"; the LLP absorb path
// passes WithDeleverageSource(DeleverageSourceLLP).
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
		// Bankrupt (maker) is risk-checked by the trade engine.
		// Deleverager (taker) is not — the post-fill HEALTHY check
		// below covers user-deleveragers, IF/Pool are absorbers.
		SkipTakerRiskCheck: true,
	}); err != nil {
		return err
	}

	// Post-fill HEALTHY check — bounds the user-supplied baseAmount
	// against the deleverager's post-state. ApplyPerpsMatching has
	// already written the new state into the store branch, so this
	// reads it directly; a pre-fill simulator would re-derive the
	// same numbers. If the assert fails, returning the error rolls
	// the branch back.
	//
	// Skipped for IF / Pool (absorbers by design).
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

