package keeper

import (
	"fmt"

	"cosmossdk.io/collections"
	"cosmossdk.io/core/store"

	"github.com/cosmos/cosmos-sdk/codec"

	"github.com/perpdex/perpdex-l1/x/liquidation/types"
)

// Keeper implements the liquidations & LLP waterfall:
//
//  1. PRE_LIQUIDATION  - flag-only; no engine action. The matching gate
//     (x/matching) restricts the user to reduce-only orders.
//  2. PARTIAL_LIQUIDATION - keeper-bot driven MsgLiquidate. The engine
//     cancels the victim's open orders, then submits a victim-owned
//     `LIQUIDATION_ORDER + IOC + reduce_only` at the zero price for
//     matching against the open book. Improvements above the zero
//     price are taxed at `min(market.LiquidationFee, price_diff_rate)`
//     and routed to the LLP / Insurance Fund. The matching loop
//     short-circuits the moment the victim is no longer in
//     liquidation, mirroring the
//     `is_not_in_liquidation_and_is_liquidation_order` guard.
//  3. FULL_LIQUIDATION - EndBlocker hands the victim's positions to
//     the LLP one at a time, ranked by ascending unrealized PnL,
//     gated by "LLP TAV stays >= LLP IMR after takeover". Any
//     positions the LLP cannot absorb fall through to ADL.
//     MsgLiquidate is intentionally NOT a keeper-bot path here:
//     PARTIAL is the only state that MsgLiquidate services. FULL and
//     BANKRUPTCY are end-block-only because they require the LLP IMR
//     gate before the LLP can take over.
//  4. BANKRUPTCY - same waterfall as FULL_LIQUIDATION. The deleverage
//     transaction (`internal_deleverage.rs`) accepts both
//     `FULL_LIQUIDATION` and `BANKRUPTCY` indistinctly; only the
//     deleverager type is filtered (IF vs user). EndBlocker therefore
//     also tries `tryLLPAbsorb` first for BANKRUPTCY victims and
//     falls through to ADL on an IMR breach. Inside ADL the
//     deleverager-side collateral assert skips under-capitalised
//     candidates and advances to the next. Any residual negative
//     collateral after a fully-deleveraged close-out remains as an
//     account-level debt on the victim ledger; the chain does NOT
//     silently move it to the IF. IF "absorption" is realised by the
//     IF taking the position via `Deleverage`, never via a silent
//     post-trade top-up.
type Keeper struct {
	cdc            codec.BinaryCodec
	storeService   store.KVStoreService
	authority      string
	accountKeeper  types.AccountKeeper
	marketKeeper   types.MarketKeeper
	riskKeeper     types.RiskKeeper
	tradeKeeper    types.TradeKeeper
	matchingKeeper types.MatchingKeeper
	fundingKeeper  types.FundingKeeper

	Schema collections.Schema
	Params collections.Item[types.Params]
	Flags  collections.Map[collections.Pair[uint64, uint32], types.LiquidationFlag]
}

func NewKeeper(cdc codec.BinaryCodec, storeService store.KVStoreService, authority string,
	ak types.AccountKeeper, mk types.MarketKeeper, rk types.RiskKeeper, tk types.TradeKeeper,
	matchk types.MatchingKeeper, fk types.FundingKeeper,
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
		fundingKeeper:  fk,

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
