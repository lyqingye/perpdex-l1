package keeper

import (
	"fmt"

	"cosmossdk.io/collections"
	"cosmossdk.io/core/store"

	"github.com/cosmos/cosmos-sdk/codec"

	"github.com/perpdex/perpdex-l1/x/asset/types"
)

// Keeper is the x/asset module keeper.
type Keeper struct {
	cdc          codec.BinaryCodec
	storeService store.KVStoreService
	authority    string

	Schema         collections.Schema
	Params         collections.Item[types.Params]
	Assets         collections.Map[uint32, types.Asset]
	DenomToIndex   collections.Map[string, uint32]
	NextAssetIndex collections.Sequence
}

// NewKeeper builds a new x/asset Keeper.
func NewKeeper(cdc codec.BinaryCodec, storeService store.KVStoreService, authority string) Keeper {
	sb := collections.NewSchemaBuilder(storeService)

	k := Keeper{
		cdc:          cdc,
		storeService: storeService,
		authority:    authority,

		Params:         collections.NewItem(sb, types.ParamsKey, "params", codec.CollValue[types.Params](cdc)),
		Assets:         collections.NewMap(sb, types.AssetKey, "assets", collections.Uint32Key, codec.CollValue[types.Asset](cdc)),
		DenomToIndex:   collections.NewMap(sb, types.DenomToIndexKey, "denom_to_index", collections.StringKey, collections.Uint32Value),
		NextAssetIndex: collections.NewSequence(sb, types.NextAssetIndexKey, "next_asset_index"),
	}

	schema, err := sb.Build()
	if err != nil {
		panic(fmt.Errorf("asset: build schema: %w", err))
	}
	k.Schema = schema
	return k
}

// Authority returns the bech32 address allowed to send governance Msg.
func (k Keeper) Authority() string { return k.authority }
