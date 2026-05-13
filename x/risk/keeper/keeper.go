package keeper

import (
	"fmt"

	"cosmossdk.io/collections"
	"cosmossdk.io/core/store"

	"github.com/cosmos/cosmos-sdk/codec"

	"github.com/perpdex/perpdex-l1/x/risk/types"
)

// Keeper implements the pure risk computations described in 16-risk.md
// and the "Liquidations & LLP" specification. The keeper owns only the
// module Params; pre-state RiskParameters used by the post-state
// regression check live in a function-local `types.PreRiskSnapshot`
// value threaded through by the caller.
//
// Mark-price reads (zero + staleness gate) live on the market keeper,
// not here; risk callers go through `k.marketKeeper.GetMarkPrice` /
// `GetMarkPriceAndDetails` so x/trade, x/matching and x/liquidation can use
// the same accessor without depending on x/risk.
type Keeper struct {
	cdc           codec.BinaryCodec
	storeService  store.KVStoreService
	authority     string
	accountKeeper types.AccountKeeper
	marketKeeper  types.MarketKeeper

	Schema collections.Schema
	Params collections.Item[types.Params]
}

func NewKeeper(cdc codec.BinaryCodec, storeService store.KVStoreService, authority string,
	ak types.AccountKeeper, mk types.MarketKeeper,
) Keeper {
	sb := collections.NewSchemaBuilder(storeService)
	k := Keeper{
		cdc:           cdc,
		storeService:  storeService,
		authority:     authority,
		accountKeeper: ak,
		marketKeeper:  mk,

		Params: collections.NewItem(sb, types.ParamsKey, "params", codec.CollValue[types.Params](cdc)),
	}
	schema, err := sb.Build()
	if err != nil {
		panic(fmt.Errorf("risk: %w", err))
	}
	k.Schema = schema
	return k
}

func (k Keeper) Authority() string { return k.authority }
