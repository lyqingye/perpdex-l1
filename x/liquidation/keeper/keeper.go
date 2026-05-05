package keeper

import (
	"context"
	"fmt"
	"sort"
	"strconv"

	"cosmossdk.io/collections"
	"cosmossdk.io/core/store"
	"cosmossdk.io/math"

	"github.com/cosmos/cosmos-sdk/codec"
	sdk "github.com/cosmos/cosmos-sdk/types"

	perptypes "github.com/perpdex/perpdex-l1/types"
	accounttypes "github.com/perpdex/perpdex-l1/x/account/types"
	"github.com/perpdex/perpdex-l1/x/liquidation/types"
	tradekeeper "github.com/perpdex/perpdex-l1/x/trade/keeper"
)

// Keeper implements the Lighter liquidations & LLP waterfall:
//
//  1. PRE_LIQUIDATION  - flag-only; no engine action. The matching gate
//     (x/matching) restricts the user to reduce-only orders.
//  2. PARTIAL_LIQUIDATION - keeper-bot driven MsgLiquidate. The engine
//     cancels the victim's open orders and books a single zero-price
//     IoC close. Any improvement over the zero price (if the
//     orderbook actually fills better in the future) is taxed up to
//     1% and routed to the LLP / Insurance Fund.
//  3. FULL_LIQUIDATION - EndBlocker hands the victim's positions to
//     the LLP one at a time, ranked by ascending unrealized PnL,
//     gated by "LLP TAV stays >= LLP IMR after takeover". Any
//     positions the LLP cannot absorb fall through to ADL.
//  4. BANKRUPTCY - skip the LLP path entirely; ADL only. The
//     insurance fund tops up the residual negative collateral.
type Keeper struct {
	cdc            codec.BinaryCodec
	storeService   store.KVStoreService
	authority      string
	accountKeeper  types.AccountKeeper
	marketKeeper   types.MarketKeeper
	riskKeeper     types.RiskKeeper
	tradeKeeper    types.TradeKeeper
	matchingKeeper types.MatchingKeeper

	Schema collections.Schema
	Params collections.Item[types.Params]
	Flags  collections.Map[collections.Pair[uint64, uint32], types.LiquidationFlag]
}

func NewKeeper(cdc codec.BinaryCodec, storeService store.KVStoreService, authority string,
	ak types.AccountKeeper, mk types.MarketKeeper, rk types.RiskKeeper, tk types.TradeKeeper,
	matchk types.MatchingKeeper,
) Keeper {
	sb := collections.NewSchemaBuilder(storeService)
	k := Keeper{
		cdc:            cdc,
		storeService:   storeService,
		authority:      authority,
		accountKeeper:  ak,
		marketKeeper:   mk,
		riskKeeper:     rk,
		tradeKeeper:    tk,
		matchingKeeper: matchk,

		Params: collections.NewItem(sb, types.ParamsKey, "params", codec.CollValue[types.Params](cdc)),
		Flags:  collections.NewMap(sb, types.LiquidationFlagKey, "flags", collections.PairKeyCodec(collections.Uint64Key, collections.Uint32Key), codec.CollValue[types.LiquidationFlag](cdc)),
	}
	schema, err := sb.Build()
	if err != nil {
		panic(fmt.Errorf("liquidation: %w", err))
	}
	k.Schema = schema
	return k
}

func (k Keeper) Authority() string { return k.authority }

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

	fill := tradekeeper.Fill{
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
		"liquidate",
		sdk.NewAttribute("victim", strconv.FormatUint(victim, 10)),
		sdk.NewAttribute("market_index", strconv.FormatUint(uint64(marketIdx), 10)),
		sdk.NewAttribute("base_amount", strconv.FormatUint(baseAmount, 10)),
		sdk.NewAttribute("zero_price", strconv.FormatUint(uint64(zeroPrice), 10)),
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
	isPoolDeleverager := dAcc.AccountType == perptypes.PublicPoolAccountType ||
		dAcc.AccountType == perptypes.InsuranceFundAccountType
	if isPoolDeleverager {
		if dAcc.PublicPoolInfo == nil ||
			dAcc.PublicPoolInfo.Status != perptypes.PublicPoolStatusActive {
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
	if err := k.tradeKeeper.ApplyPerpsMatching(ctx, tradekeeper.Fill{
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

// ApplyExitPosition is invoked by x/market when a market expires. It closes
// every open position in `marketIdx` against the insurance fund at the
// last mark price. Trades carry NoFee + NoRiskCheck so the insurance fund
// can absorb residual size even when doing so worsens its own health.
func (k Keeper) ApplyExitPosition(ctx context.Context, marketIdx uint32) error {
	md, err := k.marketKeeper.GetMarketDetails(ctx, marketIdx)
	if err != nil {
		return err
	}
	closePrice := md.MarkPrice
	if closePrice == 0 {
		// Without a mark price we cannot price the exit. Skip gracefully.
		sdk.UnwrapSDKContext(ctx).Logger().Error(
			"liquidation: skip exit position, mark price unset",
			"market", marketIdx,
		)
		return nil
	}
	sdkCtx := sdk.UnwrapSDKContext(ctx)
	closed := uint32(0)
	if err := k.accountKeeper.IterateAccounts(ctx, func(a accounttypes.Account) bool {
		if a.AccountIndex == perptypes.InsuranceFundOperatorAccountIdx {
			return false
		}
		pos, err := k.accountKeeper.GetPosition(ctx, a.AccountIndex, marketIdx)
		if err != nil || pos.Position.IsZero() {
			return false
		}
		baseAmount := pos.Position.Abs().Uint64()
		if baseAmount == 0 {
			return false
		}
		takerIsAsk := pos.Position.IsNegative()
		if err := k.tradeKeeper.ApplyPerpsMatching(ctx, tradekeeper.Fill{
			MakerAccountIndex: a.AccountIndex,
			TakerAccountIndex: perptypes.InsuranceFundOperatorAccountIdx,
			MarketIndex:       marketIdx,
			Price:             closePrice,
			BaseAmount:        baseAmount,
			IsTakerAsk:        takerIsAsk,
			NoFee:             true,
			NoRiskCheck:       true,
		}); err != nil {
			sdkCtx.Logger().Error(
				"liquidation: exit close failed",
				"market", marketIdx,
				"victim", a.AccountIndex,
				"err", err,
			)
			return false
		}
		_ = k.Flags.Remove(ctx, collections.Join(a.AccountIndex, marketIdx))
		closed++
		return false
	}); err != nil {
		return err
	}
	sdkCtx.EventManager().EmitEvent(sdk.NewEvent(
		"market_exit_position",
		sdk.NewAttribute("market_index", strconv.FormatUint(uint64(marketIdx), 10)),
		sdk.NewAttribute("close_price", strconv.FormatUint(uint64(closePrice), 10)),
		sdk.NewAttribute("closed_positions", strconv.FormatUint(uint64(closed), 10)),
	))
	return nil
}

// EndBlocker walks every account and processes liquidation in three
// stages per (account, market):
//
//  1. Flag bookkeeping. Off-chain keeper bots use these flags to
//     decide which (account, market) tuples to target with
//     MsgLiquidate. PRE / HEALTHY accounts have their flags removed.
//  2. FULL_LIQUIDATION: try to hand the position to the LLP / IF in
//     ascending uPnL order, gated by "post-takeover IF risk does not
//     breach IF IMR". Positions the IF cannot absorb fall through
//     to ADL.
//  3. BANKRUPTCY: skip the LLP path; ADL only. The chain caller
//     (EndBlocker) bounds total work by Params.MaxAdlAttemptsPerBlock.
func (k Keeper) EndBlocker(ctx context.Context) error {
	sdkCtx := sdk.UnwrapSDKContext(ctx)
	now := sdkCtx.BlockTime().UnixMilli()
	height := sdkCtx.BlockHeight()

	params, err := k.Params.Get(ctx)
	if err != nil {
		return err
	}
	attemptsLeft := params.MaxAdlAttemptsPerBlock
	candCap := params.MaxAdlCandidatesPerVictim
	if candCap == 0 {
		candCap = types.DefaultMaxADLCandidatesPerVictim
	}

	return k.accountKeeper.IterateAccounts(ctx, func(a accounttypes.Account) bool {
		if a.AccountIndex == perptypes.InsuranceFundOperatorAccountIdx {
			return false
		}
		// We process cross and isolated health independently. Cross
		// status drives the per-account flag housekeeping; per
		// isolated position is then handled via the same routine
		// against ComputeIsolatedRisk.
		if err := k.processAccount(ctx, a, height, now, &attemptsLeft, candCap); err != nil {
			sdkCtx.Logger().Error("liquidation: process account failed",
				"account", a.AccountIndex, "err", err)
		}
		return false
	})
}

// processAccount runs the per-account liquidation EndBlocker logic.
// Cross positions are flagged / liquidated against the cross health;
// each isolated position is flagged / liquidated against its own
// per-market isolated health.
func (k Keeper) processAccount(
	ctx context.Context, a accounttypes.Account, height int64, now int64,
	attemptsLeft *uint32, candCap uint32,
) error {
	sdkCtx := sdk.UnwrapSDKContext(ctx)
	crossStatus, err := k.riskKeeper.GetHealthStatus(ctx, a.AccountIndex)
	if err != nil {
		return err
	}

	// PARTIAL+: write a flag for every CROSS market this account holds
	// a position in, so off-chain keeper bots can target MsgLiquidate.
	// PRE / HEALTHY clears stale cross flags for this account's cross
	// positions.
	healthyCross := crossStatus == perptypes.HealthHealthy || crossStatus == perptypes.HealthPreLiquidation

	for marketIdx := uint32(0); marketIdx <= perptypes.MaxPerpsMarketIndex; marketIdx++ {
		pos, err := k.accountKeeper.GetPosition(ctx, a.AccountIndex, marketIdx)
		if err != nil {
			return err
		}
		if pos.Position.IsZero() {
			continue
		}
		// Determine the relevant status (cross vs isolated).
		var posStatus uint32
		if pos.MarginMode == perptypes.IsolatedMargin {
			s, err := k.riskKeeper.GetIsolatedHealthStatus(ctx, a.AccountIndex, marketIdx)
			if err != nil {
				return err
			}
			posStatus = s
		} else {
			posStatus = crossStatus
		}

		if posStatus == perptypes.HealthHealthy || posStatus == perptypes.HealthPreLiquidation {
			_ = k.Flags.Remove(ctx, collections.Join(a.AccountIndex, marketIdx))
			continue
		}

		// Flag for keeper bots.
		flag := types.LiquidationFlag{
			AccountIndex:   a.AccountIndex,
			MarketIndex:    marketIdx,
			FlaggedAtBlock: height,
			FlaggedAtTime:  now,
		}
		if err := k.Flags.Set(ctx, collections.Join(a.AccountIndex, marketIdx), flag); err != nil {
			sdkCtx.Logger().Error("liquidation: set flag failed",
				"account", a.AccountIndex, "market", marketIdx, "err", err)
		}

		// FULL_LIQUIDATION + BANKRUPTCY: try the LLP first per the
		// Lighter spec ("LLP closes all of the user's positions by
		// taking them over"), gated by SimulateRiskAfterTakeover so
		// the LLP never breaches its IMR. Anything the LLP refuses
		// falls through to ADL.
		if attemptsLeft == nil || *attemptsLeft == 0 {
			continue
		}
		if posStatus == perptypes.HealthFullLiquidation || posStatus == perptypes.HealthBankruptcy {
			absorbed, err := k.tryLLPAbsorb(ctx, a.AccountIndex, marketIdx, attemptsLeft)
			if err != nil {
				sdkCtx.Logger().Error("liquidation: LLP absorb failed",
					"victim", a.AccountIndex, "market", marketIdx, "err", err)
			}
			if !absorbed {
				if err := k.autoADL(ctx, a.AccountIndex, marketIdx, candCap, attemptsLeft); err != nil {
					sdkCtx.Logger().Error("liquidation: auto-adl failed",
						"victim", a.AccountIndex, "market", marketIdx, "err", err)
				}
			}
		}
		_ = k.absorbNegativeCollateral(ctx, a.AccountIndex)
	}

	if healthyCross {
		// Defensive: prune any stray cross-mode flags whose position
		// has since been closed (the per-loop branch above only
		// removes entries we still iterate over).
		_ = k.clearCrossFlags(ctx, a.AccountIndex)
	}
	if crossStatus != perptypes.HealthHealthy {
		sdkCtx.EventManager().EmitEvent(sdk.NewEvent(
			"liquidation_flagged",
			sdk.NewAttribute("account_index", strconv.FormatUint(a.AccountIndex, 10)),
			sdk.NewAttribute("status", strconv.FormatUint(uint64(crossStatus), 10)),
		))
	}
	return nil
}

// tryLLPAbsorb implements the Lighter "LLP picks up positions in
// ascending order of unrealized PnL, only when doing so keeps the LLP
// TAV >= IMR" rule. Called once per victim per FULL_LIQUIDATION cycle
// — it ranks the victim's OWN positions by uPnL and offers the worst
// (most negative) one to the LLP first.
//
// Returns true iff the targeted position was fully absorbed; the
// caller skips ADL on a true return. False return means the LLP would
// have breached IMR or is frozen / nonexistent — caller falls back to
// ADL for the residual size.
func (k Keeper) tryLLPAbsorb(
	ctx context.Context,
	victim uint64,
	marketIdx uint32,
	attemptsLeft *uint32,
) (bool, error) {
	if attemptsLeft == nil || *attemptsLeft == 0 {
		return false, nil
	}
	llp, err := k.accountKeeper.GetAccount(ctx, perptypes.InsuranceFundOperatorAccountIdx)
	if err != nil {
		return false, nil // IF not provisioned: silently skip.
	}
	if llp.PublicPoolInfo == nil ||
		llp.PublicPoolInfo.Status != perptypes.PublicPoolStatusActive {
		return false, nil
	}

	// Build the ranked queue of the VICTIM's positions (worst uPnL
	// first). We only attempt the targeted `marketIdx` here; the
	// outer loop walks every market in order and only invokes us
	// when this one is FULL_LIQUIDATION. We still consult the rank
	// to make sure we are not trying to absorb the BEST position
	// before the WORST has been offered — which would let the LLP
	// cherry-pick winners and leave bad positions for ADL.
	worstFirst, err := k.rankVictimPositionsByUPnL(ctx, victim)
	if err != nil {
		return false, err
	}
	if len(worstFirst) > 0 && worstFirst[0].MarketIndex != marketIdx {
		// A worse position exists in another market; defer this
		// market until that one is processed (next EndBlocker cycle
		// — accounts/markets are iterated deterministically).
		return false, nil
	}

	pos, err := k.accountKeeper.GetPosition(ctx, victim, marketIdx)
	if err != nil || pos.Position.IsZero() {
		return false, err
	}
	size := pos.Position.Abs()
	if !size.IsPositive() {
		return false, nil
	}

	// LLP IMR check: simulate the takeover and require the LLP's
	// post-state TAV >= IMR. The takeover delta is the position the
	// LLP will inherit (opposite sign of victim, since LLP is the
	// taker that offsets the victim's exposure).
	llpDelta := pos.Position.Neg()
	zeroPrice, err := k.riskKeeper.GetPositionZeroPrice(ctx, victim, marketIdx)
	if err != nil {
		return false, err
	}
	postRP, err := k.riskKeeper.SimulateRiskAfterTakeover(
		ctx, perptypes.InsuranceFundOperatorAccountIdx, marketIdx, llpDelta, zeroPrice,
	)
	if err != nil {
		return false, err
	}
	if postRP.TotalAccountValue.LT(postRP.InitialMarginRequirement) {
		// LLP would breach its initial margin; reject and let ADL
		// handle the position.
		return false, nil
	}

	if err := k.Deleverage(ctx, victim, marketIdx, perptypes.InsuranceFundOperatorAccountIdx, size.Uint64()); err != nil {
		sdk.UnwrapSDKContext(ctx).Logger().Error("liquidation: LLP absorb failed",
			"victim", victim, "market", marketIdx, "err", err)
		return false, err
	}
	*attemptsLeft--
	return true, nil
}

// rankedPosition is one row in the ranked-victim-positions list used
// by tryLLPAbsorb to enforce ascending-uPnL ordering.
type rankedPosition struct {
	MarketIndex   uint32
	UnrealizedPnL math.Int
}

// rankVictimPositionsByUPnL returns the victim's non-zero positions
// sorted by ascending unrealized PnL (worst first), as the Lighter
// spec requires for LLP takeover.
func (k Keeper) rankVictimPositionsByUPnL(ctx context.Context, victim uint64) ([]rankedPosition, error) {
	out := []rankedPosition{}
	for marketIdx := uint32(0); marketIdx <= perptypes.MaxPerpsMarketIndex; marketIdx++ {
		pos, err := k.accountKeeper.GetPosition(ctx, victim, marketIdx)
		if err != nil {
			return nil, err
		}
		if pos.Position.IsZero() {
			continue
		}
		uPnL, err := k.riskKeeper.GetPositionUnrealizedPnL(ctx, victim, marketIdx)
		if err != nil {
			// Stale oracle: skip this market in the ranking, the
			// outer EndBlocker will surface the error separately.
			continue
		}
		out = append(out, rankedPosition{MarketIndex: marketIdx, UnrealizedPnL: uPnL})
	}
	sort.Slice(out, func(i, j int) bool {
		// Ascending uPnL (most negative first); deterministic
		// market_index tiebreak.
		if !out[i].UnrealizedPnL.Equal(out[j].UnrealizedPnL) {
			return out[i].UnrealizedPnL.LT(out[j].UnrealizedPnL)
		}
		return out[i].MarketIndex < out[j].MarketIndex
	})
	return out, nil
}

// clearCrossFlags removes every (account, market) flag whose first key
// component matches `accIdx` and whose stored position is cross-mode.
// Called when the cross health is HEALTHY/PRE so stale cross flags
// from previous blocks are dropped, while leaving any isolated-mode
// flags intact (they are handled by the per-position branch above).
func (k Keeper) clearCrossFlags(ctx context.Context, accIdx uint64) error {
	rng := collections.NewPrefixedPairRange[uint64, uint32](accIdx)
	iter, err := k.Flags.Iterate(ctx, rng)
	if err != nil {
		return err
	}
	keys := []collections.Pair[uint64, uint32]{}
	for ; iter.Valid(); iter.Next() {
		k2, err := iter.Key()
		if err != nil {
			iter.Close()
			return err
		}
		keys = append(keys, k2)
	}
	iter.Close()
	for _, key := range keys {
		_, marketIdx := key.K1(), key.K2()
		pos, err := k.accountKeeper.GetPosition(ctx, accIdx, marketIdx)
		if err != nil {
			continue
		}
		if pos.MarginMode == perptypes.IsolatedMargin {
			continue
		}
		if err := k.Flags.Remove(ctx, key); err != nil {
			return err
		}
	}
	return nil
}
