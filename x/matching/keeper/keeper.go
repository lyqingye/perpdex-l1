package keeper

import (
	"fmt"

	"cosmossdk.io/collections"
	"cosmossdk.io/core/store"

	"github.com/cosmos/cosmos-sdk/codec"

	"github.com/perpdex/perpdex-l1/x/matching/types"
)

type Keeper struct {
	cdc           codec.BinaryCodec
	storeService  store.KVStoreService
	authority     string
	accountKeeper types.AccountKeeper
	marketKeeper  types.MarketKeeper
	bookKeeper    types.OrderbookKeeper
	tradeKeeper   types.TradeKeeper
	oracleKeeper  types.OracleKeeper

	Schema collections.Schema
	Params collections.Item[types.Params]
}

func NewKeeper(cdc codec.BinaryCodec, storeService store.KVStoreService, authority string,
	ak types.AccountKeeper, mk types.MarketKeeper, bk types.OrderbookKeeper, tk types.TradeKeeper,
) Keeper {
	sb := collections.NewSchemaBuilder(storeService)
	k := Keeper{
		cdc:           cdc,
		storeService:  storeService,
		authority:     authority,
		accountKeeper: ak,
		marketKeeper:  mk,
		bookKeeper:    bk,
		tradeKeeper:   tk,

		Params: collections.NewItem(sb, types.ParamsKey, "params", codec.CollValue[types.Params](cdc)),
	}
	schema, err := sb.Build()
	if err != nil {
		panic(fmt.Errorf("matching: %w", err))
	}
	k.Schema = schema
	return k
}

// SetOracleKeeper wires the oracle keeper after construction. Required for
// EndBlocker trigger resolution; the keeper is oracle-agnostic at NewKeeper
// time to avoid import cycles with modules that depend on matching.
func (k *Keeper) SetOracleKeeper(o types.OracleKeeper) { k.oracleKeeper = o }

func (k Keeper) Authority() string { return k.authority }
