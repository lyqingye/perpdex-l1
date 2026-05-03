package keeper

import (
	"context"
	"fmt"
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

type Keeper struct {
	cdc           codec.BinaryCodec
	storeService  store.KVStoreService
	authority     string
	accountKeeper types.AccountKeeper
	marketKeeper  types.MarketKeeper
	riskKeeper    types.RiskKeeper
	tradeKeeper   types.TradeKeeper

	Schema collections.Schema
	Params collections.Item[types.Params]
	Flags  collections.Map[collections.Pair[uint64, uint32], types.LiquidationFlag]
}

func NewKeeper(cdc codec.BinaryCodec, storeService store.KVStoreService, authority string,
	ak types.AccountKeeper, mk types.MarketKeeper, rk types.RiskKeeper, tk types.TradeKeeper,
) Keeper {
	sb := collections.NewSchemaBuilder(storeService)
	k := Keeper{
		cdc:           cdc,
		storeService:  storeService,
		authority:     authority,
		accountKeeper: ak,
		marketKeeper:  mk,
		riskKeeper:    rk,
		tradeKeeper:   tk,

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

// MsgLiquidate handler: validate health is in PARTIAL or FULL liquidation,
// then close the requested base amount of the victim's position via a
// liquidation fill. Insurance fund picks up any negative residual.
func (k Keeper) Liquidate(ctx context.Context, victim uint64, marketIdx uint32, baseAmount uint64, liquidatorAccount uint64) error {
	status, err := k.riskKeeper.GetHealthStatus(ctx, victim)
	if err != nil {
		return err
	}
	if status != perptypes.HealthPartialLiquidation && status != perptypes.HealthFullLiquidation {
		return types.ErrNotLiquidatable.Wrapf("status=%d", status)
	}
	pos, err := k.accountKeeper.GetPosition(ctx, victim, marketIdx)
	if err != nil {
		return err
	}
	if pos.Position.IsZero() {
		return types.ErrNotLiquidatable.Wrap("victim has no position")
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
		MakerAccountIndex: victim,
		TakerAccountIndex: liquidatorAccount,
		MarketIndex:       marketIdx,
		Price:             zeroPrice,
		BaseAmount:        baseAmount,
		IsTakerAsk:        takerIsAsk,
		TakerFee:          market.LiquidationFee,
		MakerFee:          0,
	}
	if err := k.tradeKeeper.ApplyPerpsMatching(ctx, fill); err != nil {
		return err
	}

	// Insurance fund top-up: if victim collateral is negative, withdraw from
	// the insurance fund account to cover.
	a, err := k.accountKeeper.GetAccount(ctx, victim)
	if err != nil {
		return err
	}
	if a.Collateral.IsNegative() {
		if err := k.accountKeeper.AddCollateral(ctx, perptypes.InsuranceFundOperatorAccountIdx, a.Collateral); err != nil {
			return err
		}
		if err := k.accountKeeper.AddCollateral(ctx, victim, a.Collateral.Neg()); err != nil {
			return err
		}
	}
	sdk.UnwrapSDKContext(ctx).EventManager().EmitEvent(sdk.NewEvent(
		"liquidate",
		sdk.NewAttribute("victim", strconv.FormatUint(victim, 10)),
		sdk.NewAttribute("market_index", strconv.FormatUint(uint64(marketIdx), 10)),
		sdk.NewAttribute("base_amount", strconv.FormatUint(baseAmount, 10)),
	))
	return nil
}

// Deleverage closes the victim's position at the position's zero price
// against either the insurance fund or an ADL counterparty. Mirrors the
// Lighter `internal_deleverage` invariants:
//
//   - Victim health must be FULL_LIQUIDATION or BANKRUPTCY.
//   - When deleverager == InsuranceFundOperatorAccountIdx the close-out
//     bypasses the post-trade taker risk check (insurance fund absorbs
//     residual risk regardless of its own health).
//   - When deleverager is any other account (the ADL path) the keeper
//     enforces opposite-side, size-bound and post-trade risk validity
//     to ensure a fair, opt-in-equivalent settlement.
//
// Fees are always zero for both paths.
func (k Keeper) Deleverage(ctx context.Context, victim uint64, marketIdx uint32, deleverager uint64, baseAmount uint64) error {
	status, err := k.riskKeeper.GetHealthStatus(ctx, victim)
	if err != nil {
		return err
	}
	if status != perptypes.HealthFullLiquidation && status != perptypes.HealthBankruptcy {
		return types.ErrNotBankrupt.Wrapf("status=%d", status)
	}
	if deleverager == victim {
		return types.ErrInvalidADLCounterparty.Wrap("deleverager equals victim")
	}
	pos, err := k.accountKeeper.GetPosition(ctx, victim, marketIdx)
	if err != nil {
		return err
	}
	if pos.Position.IsZero() {
		return types.ErrNotLiquidatable.Wrap("victim has no position")
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
	// Pool / IF deleveragers must be ACTIVE. Mirrors lighter
	// `internal_deleverage.rs` rejection of frozen pools.
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

	// Same sign convention as Liquidate: see comment there for the
	// derivation of `takerIsAsk = pos.Position.IsNegative()`.
	takerIsAsk := pos.Position.IsNegative()
	return k.tradeKeeper.ApplyPerpsMatching(ctx, tradekeeper.Fill{
		MakerAccountIndex: victim,
		TakerAccountIndex: deleverager,
		MarketIndex:       marketIdx,
		Price:             zeroPrice,
		BaseAmount:        baseAmount,
		IsTakerAsk:        takerIsAsk,
		NoFee:             true,
		// Insurance fund / Public Pool absorb residual risk; user ADL
		// counterparties go through the standard taker risk check
		// (their position is closing toward zero so it should always
		// pass).
		NoRiskCheck: isInsuranceFund || isPoolDeleverager,
	})
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
		// Same sign convention as Liquidate / Deleverage: takerIsAsk
		// flips the sign of the maker's existing position.
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
		// Drop any stale liquidation flag for this (account, market).
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

// EndBlocker walks every account, classifies its health and writes (or
// clears) LiquidationFlag entries so off-chain keeper bots know which
// (account, market) pairs to target with MsgLiquidate / MsgDeleverage.
//
// In addition to the flag bookkeeping, BANKRUPTCY accounts trigger an
// auto-ADL pass: if the insurance fund cannot absorb the residual loss,
// the keeper builds a profit-ranked counterparty queue and force-closes
// the victim's position against the queue, bounded by
// `Params.MaxAdlAttemptsPerBlock`. This mirrors dYdX v4's on-chain
// auto-deleveraging trigger.
//
// A flag is written for every market in which an unhealthy account holds
// a non-zero position. When the account returns to HEALTHY all flags
// owned by that account are removed.
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
		status, err := k.riskKeeper.GetHealthStatus(ctx, a.AccountIndex)
		if err != nil {
			return false
		}
		if status == perptypes.HealthHealthy || status == perptypes.HealthPreLiquidation {
			// HEALTHY / PRE_LIQUIDATION: nothing to do for keeper bots.
			// Drop any stale flags this account may have collected on a
			// previous block.
			if err := k.clearFlagsForAccount(ctx, a.AccountIndex); err != nil {
				sdkCtx.Logger().Error("liquidation: clear flags failed", "account", a.AccountIndex, "err", err)
			}
			return false
		}
		for marketIdx := uint32(0); marketIdx <= perptypes.MaxPerpsMarketIndex; marketIdx++ {
			pos, err := k.accountKeeper.GetPosition(ctx, a.AccountIndex, marketIdx)
			if err != nil || pos.Position.IsZero() {
				continue
			}
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

			if status != perptypes.HealthBankruptcy || attemptsLeft == 0 {
				continue
			}
			// IF_FIRST routing: when the IF pool is ACTIVE we hand the
			// entire residual position to it via the same Deleverage
			// (NoRiskCheck=true). The IF's risk is borne by share-
			// holders, who explicitly opted in. Frozen / nonexistent
			// IF falls back to user ADL (lighter parity with
			// `internal_deleverage` ADL queue).
			if k.tryIFAbsorb(ctx, a.AccountIndex, marketIdx, &attemptsLeft) {
				continue
			}
			if err := k.autoADL(ctx, a.AccountIndex, marketIdx, candCap, &attemptsLeft); err != nil {
				sdkCtx.Logger().Error("liquidation: auto-adl failed",
					"victim", a.AccountIndex, "market", marketIdx, "err", err)
			}
		}
		sdkCtx.EventManager().EmitEvent(sdk.NewEvent(
			"liquidation_flagged",
			sdk.NewAttribute("account_index", strconv.FormatUint(a.AccountIndex, 10)),
			sdk.NewAttribute("status", strconv.FormatUint(uint64(status), 10)),
		))
		return false
	})
}

// tryIFAbsorb hands the victim's full position in `marketIdx` to the
// Insurance Fund pool when the pool is ACTIVE. Mirrors lighter's
// IF_FIRST routing: the IF acts as the universal counterparty and its
// share-holders absorb residual loss. Returns true iff the absorb
// succeeded so the EndBlocker can skip user-ADL fallback.
func (k Keeper) tryIFAbsorb(
	ctx context.Context,
	victim uint64,
	marketIdx uint32,
	attemptsLeft *uint32,
) bool {
	if attemptsLeft == nil || *attemptsLeft == 0 {
		return false
	}
	insf, err := k.accountKeeper.GetAccount(ctx, perptypes.InsuranceFundOperatorAccountIdx)
	if err != nil {
		return false
	}
	if insf.PublicPoolInfo == nil ||
		insf.PublicPoolInfo.Status != perptypes.PublicPoolStatusActive {
		return false
	}
	pos, err := k.accountKeeper.GetPosition(ctx, victim, marketIdx)
	if err != nil || pos.Position.IsZero() {
		return false
	}
	size := pos.Position.Abs()
	if !size.IsPositive() {
		return false
	}
	if err := k.Deleverage(ctx, victim, marketIdx, perptypes.InsuranceFundOperatorAccountIdx, size.Uint64()); err != nil {
		sdk.UnwrapSDKContext(ctx).Logger().Error("liquidation: IF absorb failed",
			"victim", victim, "market", marketIdx, "err", err)
		return false
	}
	*attemptsLeft--
	return true
}

// autoADL closes a portion of the victim's `marketIdx` position against
// the top-ranked counterparties returned by BuildADLQueue. Each fill
// goes through `tradeKeeper.ApplyPerpsMatching` with NoFee+NoRiskCheck;
// candidates are profitable on the opposite side, so the close-out
// strictly improves their account value. `attemptsLeft` is decremented
// per successful fill and shared across all victims in the block.
func (k Keeper) autoADL(
	ctx context.Context,
	victim uint64,
	marketIdx uint32,
	candCap uint32,
	attemptsLeft *uint32,
) error {
	if attemptsLeft == nil || *attemptsLeft == 0 {
		return nil
	}
	pos, err := k.accountKeeper.GetPosition(ctx, victim, marketIdx)
	if err != nil {
		return err
	}
	if pos.Position.IsZero() {
		return nil
	}
	zeroPrice, err := k.riskKeeper.GetPositionZeroPrice(ctx, victim, marketIdx)
	if err != nil {
		return err
	}
	// Victim long → candidates must be short to offset (oppositeIsLong=false).
	// Victim short → candidates must be long (oppositeIsLong=true).
	oppositeIsLong := pos.Position.IsNegative()
	cands, err := k.BuildADLQueue(ctx, marketIdx, oppositeIsLong, candCap)
	if err != nil {
		return err
	}
	sdkCtx := sdk.UnwrapSDKContext(ctx)
	remaining := pos.Position.Abs()
	takerIsAsk := pos.Position.IsNegative()
	for _, c := range cands {
		if *attemptsLeft == 0 || remaining.IsZero() {
			break
		}
		size := c.PositionSize.Abs()
		if size.GT(remaining) {
			size = remaining
		}
		if !size.IsPositive() {
			continue
		}
		fill := tradekeeper.Fill{
			MakerAccountIndex: victim,
			TakerAccountIndex: c.AccountIndex,
			MarketIndex:       marketIdx,
			Price:             zeroPrice,
			BaseAmount:        size.Uint64(),
			IsTakerAsk:        takerIsAsk,
			NoFee:             true,
			// Counterparty is profitable + opposite-side; the fill
			// reduces their notional. Bypass the risk check to avoid
			// false rejections when isolated-margin edge cases would
			// otherwise block the forced close-out.
			NoRiskCheck: true,
		}
		if err := k.tradeKeeper.ApplyPerpsMatching(ctx, fill); err != nil {
			sdkCtx.Logger().Error("liquidation: auto-adl fill failed",
				"victim", victim, "market", marketIdx,
				"counterparty", c.AccountIndex, "err", err)
			continue
		}
		sdkCtx.EventManager().EmitEvent(sdk.NewEvent(
			"auto_adl",
			sdk.NewAttribute("victim", strconv.FormatUint(victim, 10)),
			sdk.NewAttribute("market_index", strconv.FormatUint(uint64(marketIdx), 10)),
			sdk.NewAttribute("counterparty", strconv.FormatUint(c.AccountIndex, 10)),
			sdk.NewAttribute("base_amount", strconv.FormatUint(size.Uint64(), 10)),
			sdk.NewAttribute("price", strconv.FormatUint(uint64(zeroPrice), 10)),
		))
		remaining = remaining.Sub(size)
		*attemptsLeft--
	}
	return nil
}

// clearFlagsForAccount removes every (account, market) flag whose first
// key component matches `accIdx`.
func (k Keeper) clearFlagsForAccount(ctx context.Context, accIdx uint64) error {
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
		if err := k.Flags.Remove(ctx, key); err != nil {
			return err
		}
	}
	return nil
}
