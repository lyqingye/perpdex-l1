package keeper

import (
	"fmt"

	"cosmossdk.io/collections"
	"cosmossdk.io/core/store"

	"github.com/cosmos/cosmos-sdk/codec"

	"github.com/perpdex/perpdex-l1/x/liquidation/types"
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
