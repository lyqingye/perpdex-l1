package keeper

import (
	"context"
	"fmt"
	"strconv"

	"cosmossdk.io/collections"
	"cosmossdk.io/core/store"

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

// Deleverage closes the victim's position against the deleverager at zero
// price, with no fees. Used in BANKRUPTCY state when the insurance fund cannot
// absorb further losses.
func (k Keeper) Deleverage(ctx context.Context, victim uint64, marketIdx uint32, deleverager uint64, baseAmount uint64) error {
	status, err := k.riskKeeper.GetHealthStatus(ctx, victim)
	if err != nil {
		return err
	}
	if status != perptypes.HealthBankruptcy {
		return types.ErrNotBankrupt.Wrapf("status=%d", status)
	}
	pos, err := k.accountKeeper.GetPosition(ctx, victim, marketIdx)
	if err != nil {
		return err
	}
	zeroPrice, err := k.riskKeeper.GetPositionZeroPrice(ctx, victim, marketIdx)
	if err != nil {
		return err
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
// A flag is written for every market in which an unhealthy account holds
// a non-zero position. When the account returns to HEALTHY all flags
// owned by that account are removed.
func (k Keeper) EndBlocker(ctx context.Context) error {
	sdkCtx := sdk.UnwrapSDKContext(ctx)
	now := sdkCtx.BlockTime().UnixMilli()
	height := sdkCtx.BlockHeight()
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
		}
		sdkCtx.EventManager().EmitEvent(sdk.NewEvent(
			"liquidation_flagged",
			sdk.NewAttribute("account_index", strconv.FormatUint(a.AccountIndex, 10)),
			sdk.NewAttribute("status", strconv.FormatUint(uint64(status), 10)),
		))
		return false
	})
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
