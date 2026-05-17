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

// Canonical source values on EventTypeDeleverage.
const (
	DeleverageSourceMsg     = "msg"
	DeleverageSourceLLP     = "llp"
	DeleverageSourceAutoADL = "auto_adl"
)

// DeleverageOption tags the entry point on the emitted event.
type DeleverageOption func(*deleverageOpts)

type deleverageOpts struct {
	source string
}

// WithDeleverageSource sets the source attribute (DeleverageSource*).
func WithDeleverageSource(source string) DeleverageOption {
	return func(o *deleverageOpts) { o.source = source }
}

// Liquidate is the MsgLiquidate entry point (PARTIAL only):
//  1. require PARTIAL_LIQUIDATION (FULL/BANKRUPTCY are EndBlocker
//     territory)
//  2. cancel the victim's open orders so they cannot front-run the
//     close-out
//  3. compute the zero price (TAV/MMR-invariant)
//  4. submit a synthetic LIQUIDATION_ORDER + IOC + reduce_only on
//     behalf of the victim; improvements above ZP are taxed at
//     min(market.LiquidationFee, price_diff_rate) → LLP/IF
//
// No post-trade IF top-up: fills happen at >= ZP and the fee is
// taxed only on the improvement, so collateral cannot go negative
// via this path. IF absorption only happens in FULL/BANKRUPTCY via
// Deleverage (gated by tryLLPAbsorb).
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
	// MsgLiquidate is PARTIAL-only; FULL/BANKRUPTCY route through
	// EndBlocker (see abci.go).
	if status != perptypes.HealthPartialLiquidation {
		return types.ErrNotLiquidatable.Wrapf(
			"victim status=%d not partial; FULL/BANKRUPTCY routes via EndBlocker LLP→ADL",
			status,
		)
	}
	if baseAmount == 0 {
		return types.ErrInvalidParams.Wrap("base_amount must be > 0")
	}
	// Cap baseAmount to remaining size: overshoot would flip the
	// position and steal collateral. Symmetric to Deleverage.
	absVictim := pos.BaseSize.Abs()
	if math.NewIntFromUint64(baseAmount).GT(absVictim) {
		return types.ErrInvalidParams.Wrapf(
			"base_amount=%d exceeds victim position size %s", baseAmount, absVictim.String(),
		)
	}
	// Cancel-all BEFORE the IOC so resting orders cannot front-run.
	// A nil matchingKeeper is tolerated only for stub-driven tests.
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

// Deleverage is the MsgDeleverage entry point and the LLP-takeover
// path called from EndBlocker. autoADL does NOT route through here —
// it drives the trade engine directly at ZeroPriceMid.
//
// Risk model:
//   - Bankrupt maker: trade engine's IsValidRiskChangeFrom (NOT
//     SkipMakerRiskCheck).
//   - Bankrupt collateral sufficiency is NOT asserted: the ZP
//     formula produces extreme prices for deeply-bankrupt accounts;
//     residual negative collateral persists as ledger debt.
//   - User deleverager: must remain HEALTHY after the fill — the
//     post-fill check below reads real post-state and rolls back on
//     failure.
//   - IF / Pool deleveragers skip the post-fill check (absorbers by
//     design; LLP IMR gate is upstream).
//
// SkipTakerRiskCheck=true lets the post-fill HEALTHY check below own
// deleverager-side enforcement (avoids a redundant engine read).
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
		// User-picked counterparty needs the same-side / size-cap
		// guard. autoADL bypasses this function.
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
		// Maker checked by engine; taker checked below for users.
		SkipTakerRiskCheck: true,
	}); err != nil {
		return err
	}

	// Post-fill HEALTHY check — bounds the user-supplied baseAmount.
	// Reads the post-state ApplyPerpsMatching already wrote; an
	// error rolls back the store branch. IF/Pool are absorbers.
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

	// No IF top-up: residual negative collateral persists as debt.
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

// healthEnvelopeFor returns the correct health status for a position:
// cross aggregate for cross, per-market for isolated. Single
// definition shared by Liquidate / Deleverage / processAccount.
func (k Keeper) healthEnvelopeFor(
	ctx context.Context, accIdx uint64, marketIdx uint32, marginMode uint32,
) (uint32, error) {
	if marginMode == perptypes.IsolatedMargin {
		return k.riskKeeper.GetIsolatedHealthStatus(ctx, accIdx, marketIdx)
	}
	return k.riskKeeper.GetHealthStatus(ctx, accIdx)
}

