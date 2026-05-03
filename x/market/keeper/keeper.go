package keeper

import (
	"context"
	"errors"
	"fmt"

	"cosmossdk.io/collections"
	"cosmossdk.io/core/store"
	"cosmossdk.io/math"

	"github.com/cosmos/cosmos-sdk/codec"

	perptypes "github.com/perpdex/perpdex-l1/types"
	"github.com/perpdex/perpdex-l1/x/market/types"
)

type Keeper struct {
	cdc          codec.BinaryCodec
	storeService store.KVStoreService
	authority    string

	assetKeeper       types.AssetKeeper
	liquidationKeeper types.LiquidationKeeper

	Schema        collections.Schema
	Params        collections.Item[types.Params]
	Markets       collections.Map[uint32, types.Market]
	MarketDetails collections.Map[uint32, types.MarketDetails]
	ExpiryIndex   collections.KeySet[collections.Pair[int64, uint32]]
}

func NewKeeper(cdc codec.BinaryCodec, storeService store.KVStoreService, authority string, assetK types.AssetKeeper) Keeper {
	sb := collections.NewSchemaBuilder(storeService)
	k := Keeper{
		cdc:          cdc,
		storeService: storeService,
		authority:    authority,
		assetKeeper:  assetK,

		Params:        collections.NewItem(sb, types.ParamsKey, "params", codec.CollValue[types.Params](cdc)),
		Markets:       collections.NewMap(sb, types.MarketKey, "markets", collections.Uint32Key, codec.CollValue[types.Market](cdc)),
		MarketDetails: collections.NewMap(sb, types.MarketDetailsKey, "market_details", collections.Uint32Key, codec.CollValue[types.MarketDetails](cdc)),
		ExpiryIndex:   collections.NewKeySet(sb, types.ExpiryIndexKey, "expiry_index", collections.PairKeyCodec(collections.Int64Key, collections.Uint32Key)),
	}
	schema, err := sb.Build()
	if err != nil {
		panic(fmt.Errorf("market: %w", err))
	}
	k.Schema = schema
	return k
}

func (k *Keeper) SetLiquidationKeeper(l types.LiquidationKeeper) { k.liquidationKeeper = l }

func (k Keeper) Authority() string { return k.authority }

// GetMarket returns a Market or ErrMarketNotFound.
func (k Keeper) GetMarket(ctx context.Context, idx uint32) (types.Market, error) {
	m, err := k.Markets.Get(ctx, idx)
	if err != nil {
		if errors.Is(err, collections.ErrNotFound) {
			return types.Market{}, types.ErrMarketNotFound.Wrapf("market_index=%d", idx)
		}
		return types.Market{}, err
	}
	return m, nil
}

// GetMarketDetails returns a MarketDetails or ErrMarketNotFound.
func (k Keeper) GetMarketDetails(ctx context.Context, idx uint32) (types.MarketDetails, error) {
	d, err := k.MarketDetails.Get(ctx, idx)
	if err != nil {
		if errors.Is(err, collections.ErrNotFound) {
			return types.MarketDetails{}, types.ErrMarketNotFound.Wrapf("market_details_index=%d", idx)
		}
		return types.MarketDetails{}, err
	}
	if d.FundingRatePrefixSum.IsNil() {
		d.FundingRatePrefixSum = math.ZeroInt()
	}
	return d, nil
}

func (k Keeper) SetMarket(ctx context.Context, m types.Market) error {
	if err := k.Markets.Set(ctx, m.MarketIndex, m); err != nil {
		return err
	}
	if m.ExpiryTimestamp > 0 {
		_ = k.ExpiryIndex.Set(ctx, collections.Join(m.ExpiryTimestamp, m.MarketIndex))
	}
	return nil
}

func (k Keeper) SetMarketDetails(ctx context.Context, d types.MarketDetails) error {
	return k.MarketDetails.Set(ctx, d.MarketIndex, d)
}

// AllocateNonce returns the next ask or bid nonce for the market and persists
// the new value. ask increments, bid decrements; the call returns
// ErrNonceExhausted if ask>=bid would result.
func (k Keeper) AllocateNonce(ctx context.Context, marketIdx uint32, isAsk bool) (int64, error) {
	d, err := k.GetMarketDetails(ctx, marketIdx)
	if err != nil {
		return 0, err
	}
	if d.AskNonce >= d.BidNonce {
		return 0, types.ErrNonceExhausted
	}
	var nonce int64
	if isAsk {
		nonce = d.AskNonce
		d.AskNonce++
	} else {
		nonce = d.BidNonce
		d.BidNonce--
	}
	if d.AskNonce >= d.BidNonce {
		return 0, types.ErrNonceExhausted
	}
	if err := k.SetMarketDetails(ctx, d); err != nil {
		return 0, err
	}
	return nonce, nil
}

// UpdateOpenInterest applies a signed delta to a market's open interest and
// fails when the configured limit is exceeded.
func (k Keeper) UpdateOpenInterest(ctx context.Context, marketIdx uint32, delta int64) error {
	d, err := k.GetMarketDetails(ctx, marketIdx)
	if err != nil {
		return err
	}
	d.OpenInterest += delta
	if d.OpenInterest < 0 {
		d.OpenInterest = 0
	}
	if d.OpenInterestLimit > 0 && uint64(d.OpenInterest) > d.OpenInterestLimit {
		return types.ErrOpenInterestLimit
	}
	return k.SetMarketDetails(ctx, d)
}

// IsPerpsMarket reports whether the market index falls in the perpetual range.
func (k Keeper) IsPerpsMarket(idx uint32) bool {
	return idx <= perptypes.MaxPerpsMarketIndex
}

// IsSpotMarket reports whether the market index falls in the spot range.
func (k Keeper) IsSpotMarket(idx uint32) bool {
	return idx >= perptypes.MinSpotMarketIndex && idx <= perptypes.MaxSpotMarketIndex
}

// IterateMarkets walks all markets in index order.
func (k Keeper) IterateMarkets(ctx context.Context, cb func(types.Market) bool) error {
	iter, err := k.Markets.Iterate(ctx, nil)
	if err != nil {
		return err
	}
	defer iter.Close()
	for ; iter.Valid(); iter.Next() {
		v, err := iter.Value()
		if err != nil {
			return err
		}
		if cb(v) {
			return nil
		}
	}
	return nil
}
