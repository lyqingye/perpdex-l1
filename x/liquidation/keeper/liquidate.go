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
//  1. Verify the victim is in PARTIAL or FULL liquidation.
//  2. Cancel every open order owned by the victim. A victim's resting
//     bids could otherwise front-run the close-out fill.
//  3. Compute the position's mark-based zero price (TAV/MMR ratio
//     invariant).
//  4. Issue a single fill at the zero price; route any improvement-
//     based fee to the LLP / Insurance Fund (capped at 1% of notional).
//  5. Top up any residual negative collateral from the Insurance Fund.
func (k Keeper) Liquidate(ctx context.Context, victim uint64, marketIdx uint32, baseAmount uint64, liquidatorAccount uint64) error {
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
	if status != perptypes.HealthPartialLiquidation && status != perptypes.HealthFullLiquidation {
		return types.ErrNotLiquidatable.Wrapf("status=%d", status)
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
	// Cancel-all orders BEFORE booking the close to mirror lighter's
	// "cancel all open orders of the user" step. We tolerate failure
	// when the matching keeper is not wired (tests).
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

	// Victim is the maker; the trade keeper convention is:
	//   IsTakerAsk=true  ⇒ makerSign=+1 (maker buys / increases long)
	//   IsTakerAsk=false ⇒ makerSign=-1 (maker sells / increases short)
	// To CLOSE the victim's position we therefore need the maker delta
	// to flip the sign of the existing position: long victim → maker
	// delta negative → IsTakerAsk=false; short victim → maker delta
	// positive → IsTakerAsk=true. That is `pos.Position.IsNegative()`.
	takerIsAsk := pos.Position.IsNegative()

	fill := tradekeeper.PerpFill{
		MakerAccountIndex:       victim,
		TakerAccountIndex:       liquidatorAccount,
		MarketIndex:             marketIdx,
		Price:                   zeroPrice,
		BaseAmount:              baseAmount,
		IsTakerAsk:              takerIsAsk,
		// Standard taker/maker fees suppressed; only the
		// improvement-over-zero-price fee applies on the close-out.
		TakerFee:                0,
		MakerFee:                0,
		ZeroPrice:               zeroPrice,
		LiquidationFeeBps:       market.LiquidationFee,
		LiquidationFeeRecipient: perptypes.InsuranceFundOperatorAccountIdx,
		// Victim is being closed at zero price by construction; the
		// fill mechanically improves their TAV/MMR ratio. Skip the
		// post-trade risk check on the victim/maker side so a still-
		// unhealthy post-state is not rejected back into the
		// keeper-bot loop.
		SkipMakerRiskCheck: true,
	}
	if err := k.tradeKeeper.ApplyPerpsMatching(ctx, fill); err != nil {
		return err
	}

	if err := k.absorbNegativeCollateral(ctx, victim); err != nil {
		return err
	}
	sdk.UnwrapSDKContext(ctx).EventManager().EmitEvent(sdk.NewEvent(
		types.EventTypeLiquidate,
		sdk.NewAttribute(types.AttributeKeyVictim, strconv.FormatUint(victim, 10)),
		sdk.NewAttribute(types.AttributeKeyMarketIndex, strconv.FormatUint(uint64(marketIdx), 10)),
		sdk.NewAttribute(types.AttributeKeyBaseAmount, strconv.FormatUint(baseAmount, 10)),
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
