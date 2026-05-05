package keeper

import (
	"context"
	"errors"
	"fmt"

	"cosmossdk.io/collections"
	"cosmossdk.io/core/store"

	"github.com/cosmos/cosmos-sdk/codec"
	sdk "github.com/cosmos/cosmos-sdk/types"

	"github.com/perpdex/perpdex-l1/x/oracle/types"
)

// Keeper owns the oracle store and the late-bound dependencies required by
// the dydx/Slinky-style ABCI++ vote-extension pipeline.
//
// `stakingKeeper` is consulted by PrepareProposal / ProcessProposal to map
// each cometbft consensus address to a validator and look up its voting
// power. `priceFetcher` is consulted by ExtendVote on a validator's local
// node to source the latest mark/index prices (typically a sidecar).
//
// Both are stored via pointer-to-interface so that `Keeper` can still be
// passed around by value (the cosmos-sdk convention) while late-binding
// updates remain visible to every copy.
type Keeper struct {
	cdc          codec.BinaryCodec
	storeService store.KVStoreService
	authority    string

	stakingKeeperHolder *types.StakingKeeper
	priceFetcherHolder  *PriceFetcher

	Schema collections.Schema
	Params collections.Item[types.Params]
	Prices collections.Map[uint32, types.OraclePrice]
}

// NewKeeper builds a fresh keeper. The staking keeper / price fetcher
// must be injected post-construction via SetStakingKeeper / SetPriceFetcher
// because the staking keeper is itself constructed later in app wiring
// and the price fetcher is supplied by app options.
func NewKeeper(cdc codec.BinaryCodec, storeService store.KVStoreService, authority string) Keeper {
	sb := collections.NewSchemaBuilder(storeService)
	var noop PriceFetcher = noopPriceFetcher{}
	var staking types.StakingKeeper
	k := Keeper{
		cdc:                 cdc,
		storeService:        storeService,
		authority:           authority,
		priceFetcherHolder:  &noop,
		stakingKeeperHolder: &staking,

		Params: collections.NewItem(sb, types.ParamsKey, "params", codec.CollValue[types.Params](cdc)),
		Prices: collections.NewMap(sb, types.PriceKey, "prices", collections.Uint32Key, codec.CollValue[types.OraclePrice](cdc)),
	}
	schema, err := sb.Build()
	if err != nil {
		panic(fmt.Errorf("oracle: %w", err))
	}
	k.Schema = schema
	return k
}

// Authority returns the gov module address that gates `MsgUpdateParams`
// and is used as the signer of the proposer-injected
// `MsgAggregateOracleVotes` transaction.
func (k Keeper) Authority() string { return k.authority }

// SetStakingKeeper wires the staking keeper after app construction.
// Must be called before the first block that exercises the VE pipeline.
func (k Keeper) SetStakingKeeper(sk types.StakingKeeper) { *k.stakingKeeperHolder = sk }

// SetPriceFetcher injects the local-node PriceFetcher. The default is a
// no-op that returns an empty price set, which keeps unit tests and
// validators without a sidecar from breaking consensus (their VE will
// simply contribute nothing to the median).
func (k Keeper) SetPriceFetcher(f PriceFetcher) {
	if f == nil {
		*k.priceFetcherHolder = noopPriceFetcher{}
		return
	}
	*k.priceFetcherHolder = f
}

// StakingKeeper returns the wired staking keeper. Used by the VE handler
// and tests; nil-safe consumers must check.
func (k Keeper) StakingKeeper() types.StakingKeeper { return *k.stakingKeeperHolder }

// PriceFetcher returns the wired price fetcher (never nil after NewKeeper).
func (k Keeper) PriceFetcher() PriceFetcher { return *k.priceFetcherHolder }

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

// GetFreshPrice is a staleness-aware accessor used by risk / liquidation /
// funding. It refuses prices whose `LastUpdatedTimestamp` is older than
// `Params.MaxAgeMs`. A zero `LastUpdatedTimestamp` is treated as stale to
// avoid counting genesis-seeded placeholders as live.
func (k Keeper) GetFreshPrice(ctx context.Context, marketIdx uint32) (types.OraclePrice, error) {
	p, err := k.GetPrice(ctx, marketIdx)
	if err != nil {
		return types.OraclePrice{}, err
	}
	params, err := k.Params.Get(ctx)
	if err != nil {
		return types.OraclePrice{}, err
	}
	if params.MaxAgeMs <= 0 {
		return p, nil
	}
	now := sdk.UnwrapSDKContext(ctx).BlockTime().UnixMilli()
	if p.LastUpdatedTimestamp <= 0 {
		return types.OraclePrice{}, types.ErrStalePrice.Wrapf(
			"market_index=%d never updated", marketIdx,
		)
	}
	if now-p.LastUpdatedTimestamp > params.MaxAgeMs {
		return types.OraclePrice{}, types.ErrStalePrice.Wrapf(
			"market_index=%d age=%dms > max=%dms",
			marketIdx, now-p.LastUpdatedTimestamp, params.MaxAgeMs,
		)
	}
	return p, nil
}

// SetPrice stores an oracle price for a market. Zero index/mark prices are
// rejected because they always signal a broken upstream (genesis only uses
// SetPriceUnsafe via InitGenesis).
func (k Keeper) SetPrice(ctx context.Context, p types.OraclePrice) error {
	if p.IndexPrice == 0 || p.MarkPrice == 0 {
		return types.ErrInvalidPrice.Wrapf(
			"prices must be non-zero (index=%d mark=%d)",
			p.IndexPrice, p.MarkPrice,
		)
	}
	return k.Prices.Set(ctx, p.MarketIndex, p)
}

// SetPriceUnsafe bypasses the non-zero check and is intended for genesis
// loading and test fixtures only. Runtime paths (vote-extension aggregate)
// must use SetPrice so zero prices can never survive a block.
func (k Keeper) SetPriceUnsafe(ctx context.Context, p types.OraclePrice) error {
	return k.Prices.Set(ctx, p.MarketIndex, p)
}
