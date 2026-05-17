package keeper

import (
	"fmt"

	"cosmossdk.io/collections"
	"cosmossdk.io/core/store"

	"github.com/cosmos/cosmos-sdk/codec"

	"github.com/perpdex/perpdex-l1/x/liquidation/types"
)

// Keeper implements the liquidation & LLP waterfall:
//
//  1. PRE_LIQUIDATION  – no engine action; x/matching restricts the
//     user to reduce-only orders.
//  2. PARTIAL_LIQUIDATION – keeper-bot driven MsgLiquidate. Cancels
//     the victim's orders, then submits a victim-owned
//     LIQUIDATION_ORDER + IOC + reduce_only at the zero price.
//     Improvements over zero are taxed at
//     min(market.LiquidationFee, price_diff_rate) and routed to
//     LLP / IF. Matching short-circuits the moment the victim is no
//     longer in liquidation.
//  3. FULL_LIQUIDATION – EndBlocker hands positions to the LLP in
//     ascending-uPnL order, gated by "LLP TAV >= LLP IMR after
//     takeover"; positions the LLP cannot absorb fall through to ADL.
//  4. BANKRUPTCY – same waterfall as FULL. Residual negative
//     collateral after a fully-deleveraged close-out stays on the
//     victim ledger as account-level debt; IF "absorption" is the
//     IF taking the position via Deleverage, never a silent top-up.
//
// MsgLiquidate only services PARTIAL; FULL/BANKRUPTCY route through
// EndBlocker because they require the LLP IMR gate.
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
	}
	schema, err := sb.Build()
	if err != nil {
		panic(fmt.Errorf("liquidation: %w", err))
	}
	k.Schema = schema
	return k
}

func (k Keeper) Authority() string { return k.authority }
