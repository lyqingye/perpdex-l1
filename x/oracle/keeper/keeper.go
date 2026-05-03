package keeper

import (
	"context"
	"errors"
	"fmt"

	"cosmossdk.io/collections"
	"cosmossdk.io/core/store"

	"github.com/cosmos/cosmos-sdk/codec"

	"github.com/perpdex/perpdex-l1/x/oracle/types"
)

type Keeper struct {
	cdc          codec.BinaryCodec
	storeService store.KVStoreService
	authority    string

	stakingKeeper  types.StakingKeeper
	slashingKeeper types.SlashingKeeper

	Schema     collections.Schema
	Params     collections.Item[types.Params]
	Prices     collections.Map[uint32, types.OraclePrice]
	Providers  collections.Map[string, types.OracleProvider]
	Bindings   collections.Map[string, types.ValidatorOracleBinding]
	OperatorIdx collections.Map[string, string]
	Stats      collections.Map[string, types.ValidatorOracleStats]
	Epoch      collections.Item[int64]
}

func NewKeeper(cdc codec.BinaryCodec, storeService store.KVStoreService, authority string, sk types.StakingKeeper, slk types.SlashingKeeper) Keeper {
	sb := collections.NewSchemaBuilder(storeService)
	k := Keeper{
		cdc:            cdc,
		storeService:   storeService,
		authority:      authority,
		stakingKeeper:  sk,
		slashingKeeper: slk,

		Params:      collections.NewItem(sb, types.ParamsKey, "params", codec.CollValue[types.Params](cdc)),
		Prices:      collections.NewMap(sb, types.PriceKey, "prices", collections.Uint32Key, codec.CollValue[types.OraclePrice](cdc)),
		Providers:   collections.NewMap(sb, types.ProviderKey, "providers", collections.StringKey, codec.CollValue[types.OracleProvider](cdc)),
		Bindings:    collections.NewMap(sb, types.BindingKey, "bindings", collections.StringKey, codec.CollValue[types.ValidatorOracleBinding](cdc)),
		OperatorIdx: collections.NewMap(sb, types.OperatorIdxKey, "operator_index", collections.StringKey, collections.StringValue),
		Stats:       collections.NewMap(sb, types.StatsKey, "stats", collections.StringKey, codec.CollValue[types.ValidatorOracleStats](cdc)),
		Epoch:       collections.NewItem(sb, types.EpochKey, "epoch", collections.Int64Value),
	}
	schema, err := sb.Build()
	if err != nil {
		panic(fmt.Errorf("oracle: %w", err))
	}
	k.Schema = schema
	return k
}

func (k Keeper) Authority() string { return k.authority }

// GetPrice returns the current oracle price for a market or ErrPriceNotFound.
func (k Keeper) GetPrice(ctx context.Context, marketIdx uint32) (types.OraclePrice, error) {
	p, err := k.Prices.Get(ctx, marketIdx)
	if err != nil {
		if errors.Is(err, collections.ErrNotFound) {
			return types.OraclePrice{}, types.ErrPriceNotFound.Wrapf("market_index=%d", marketIdx)
		}
		return types.OraclePrice{}, err
	}
	return p, nil
}

// SetPrice stores an oracle price for a market.
func (k Keeper) SetPrice(ctx context.Context, p types.OraclePrice) error {
	return k.Prices.Set(ctx, p.MarketIndex, p)
}

// AllProviders returns the list of registered oracle providers.
func (k Keeper) AllProviders(ctx context.Context) ([]types.OracleProvider, error) {
	out := []types.OracleProvider{}
	iter, err := k.Providers.Iterate(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer iter.Close()
	for ; iter.Valid(); iter.Next() {
		v, err := iter.Value()
		if err != nil {
			return nil, err
		}
		out = append(out, v)
	}
	return out, nil
}

// AllBindings returns every validator->oracle operator binding.
func (k Keeper) AllBindings(ctx context.Context) ([]types.ValidatorOracleBinding, error) {
	out := []types.ValidatorOracleBinding{}
	iter, err := k.Bindings.Iterate(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer iter.Close()
	for ; iter.Valid(); iter.Next() {
		v, err := iter.Value()
		if err != nil {
			return nil, err
		}
		out = append(out, v)
	}
	return out, nil
}
