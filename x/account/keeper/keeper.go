package keeper

import (
	"fmt"

	"cosmossdk.io/collections"
	"cosmossdk.io/core/store"
	"cosmossdk.io/log"

	"github.com/cosmos/cosmos-sdk/codec"

	"github.com/perpdex/perpdex-l1/x/account/types"
)

// Keeper is the x/account keeper.
type Keeper struct {
	cdc          codec.BinaryCodec
	storeService store.KVStoreService
	authority    string

	assetKeeper types.AssetKeeper
	bankKeeper  types.BankKeeper
	// Optional / wired post-construction:
	fundingKeeper types.FundingKeeper
	riskKeeper    types.RiskKeeper
	marketKeeper  types.MarketKeeper

	// State.
	Schema           collections.Schema
	Params           collections.Item[types.Params]
	Accounts         collections.Map[uint64, types.Account]
	OwnerToIndex     collections.Map[string, uint64]
	AccountAssets    collections.Map[collections.Pair[uint64, uint32], types.AccountAsset]
	AccountPositions collections.Map[collections.Pair[uint64, uint32], types.AccountPosition]
	AccountMetas     collections.Map[uint64, types.AccountMeta]
	NextMasterIndex  collections.Sequence
	NextSubIndex     collections.Sequence
}

// NewKeeper builds the x/account Keeper.
func NewKeeper(
	cdc codec.BinaryCodec,
	storeService store.KVStoreService,
	authority string,
	assetK types.AssetKeeper,
	bankK types.BankKeeper,
) Keeper {
	sb := collections.NewSchemaBuilder(storeService)

	k := Keeper{
		cdc:          cdc,
		storeService: storeService,
		authority:    authority,
		assetKeeper:  assetK,
		bankKeeper:   bankK,

		Params:           collections.NewItem(sb, types.ParamsKey, "params", codec.CollValue[types.Params](cdc)),
		Accounts:         collections.NewMap(sb, types.AccountKey, "accounts", collections.Uint64Key, codec.CollValue[types.Account](cdc)),
		OwnerToIndex:     collections.NewMap(sb, types.OwnerToIndexKey, "owner_to_index", collections.StringKey, collections.Uint64Value),
		AccountAssets:    collections.NewMap(sb, types.AccountAssetKey, "account_assets", collections.PairKeyCodec(collections.Uint64Key, collections.Uint32Key), codec.CollValue[types.AccountAsset](cdc)),
		AccountPositions: collections.NewMap(sb, types.AccountPositionKey, "account_positions", collections.PairKeyCodec(collections.Uint64Key, collections.Uint32Key), codec.CollValue[types.AccountPosition](cdc)),
		AccountMetas:     collections.NewMap(sb, types.AccountMetaKey, "account_metas", collections.Uint64Key, codec.CollValue[types.AccountMeta](cdc)),
		NextMasterIndex:  collections.NewSequence(sb, types.NextMasterIndexKey, "next_master_index"),
		NextSubIndex:     collections.NewSequence(sb, types.NextSubIndexKey, "next_sub_index"),
	}

	schema, err := sb.Build()
	if err != nil {
		panic(fmt.Errorf("account: build schema: %w", err))
	}
	k.Schema = schema
	return k
}

// SetFundingKeeper allows late-binding the funding keeper to break a cycle.
func (k *Keeper) SetFundingKeeper(f types.FundingKeeper) { k.fundingKeeper = f }

// SetRiskKeeper allows late-binding the risk keeper to break a cycle.
func (k *Keeper) SetRiskKeeper(r types.RiskKeeper) { k.riskKeeper = r }

// SetMarketKeeper allows late-binding the market keeper (late because the
// market keeper is built after account keeper during wiring).
func (k *Keeper) SetMarketKeeper(m types.MarketKeeper) { k.marketKeeper = m }

// AssetKeeper returns the wired asset keeper for cross-module use.
func (k Keeper) AssetKeeper() types.AssetKeeper { return k.assetKeeper }

func (k Keeper) Authority() string { return k.authority }

func (k Keeper) Logger(ctx interface{ Logger() log.Logger }) log.Logger {
	return ctx.Logger().With("module", "x/"+types.ModuleName)
}
