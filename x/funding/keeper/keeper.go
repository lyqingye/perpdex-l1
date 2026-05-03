package keeper

import (
	"context"
	"fmt"

	"cosmossdk.io/collections"
	"cosmossdk.io/core/store"
	"cosmossdk.io/math"

	"github.com/cosmos/cosmos-sdk/codec"

	perptypes "github.com/perpdex/perpdex-l1/types"
	"github.com/perpdex/perpdex-l1/x/funding/types"
)

type Keeper struct {
	cdc           codec.BinaryCodec
	storeService  store.KVStoreService
	authority     string
	marketKeeper  types.MarketKeeper
	oracleKeeper  types.OracleKeeper
	bookKeeper    types.OrderbookKeeper
	accountKeeper types.AccountKeeper

	Schema   collections.Schema
	Params   collections.Item[types.Params]
	Metadata collections.Item[types.FundingMetadata]
}

func NewKeeper(cdc codec.BinaryCodec, storeService store.KVStoreService, authority string,
	mk types.MarketKeeper, ok types.OracleKeeper, bk types.OrderbookKeeper, ak types.AccountKeeper,
) Keeper {
	sb := collections.NewSchemaBuilder(storeService)
	k := Keeper{
		cdc:           cdc,
		storeService:  storeService,
		authority:     authority,
		marketKeeper:  mk,
		oracleKeeper:  ok,
		bookKeeper:    bk,
		accountKeeper: ak,

		Params:   collections.NewItem(sb, types.ParamsKey, "params", codec.CollValue[types.Params](cdc)),
		Metadata: collections.NewItem(sb, types.MetadataKey, "metadata", codec.CollValue[types.FundingMetadata](cdc)),
	}
	schema, err := sb.Build()
	if err != nil {
		panic(fmt.Errorf("funding: %w", err))
	}
	k.Schema = schema
	return k
}

func (k Keeper) Authority() string { return k.authority }

// SettlePositionFunding applies the difference between the global
// funding_rate_prefix_sum and the position's last_funding_rate_prefix_sum to
// the position collateral, then snapshots the new prefix sum.
//
// Returns (delta, nil) where delta is positive when the position gains and
// negative when it pays funding.
func (k Keeper) SettlePositionFunding(ctx context.Context, accountIndex uint64, marketIndex uint32) error {
	pos, err := k.accountKeeper.GetPosition(ctx, accountIndex, marketIndex)
	if err != nil {
		return err
	}
	if pos.Position.IsZero() {
		return nil
	}
	d, err := k.marketKeeper.GetMarketDetails(ctx, marketIndex)
	if err != nil {
		return err
	}
	delta := d.FundingRatePrefixSum.Sub(pos.LastFundingRatePrefixSum)
	if delta.IsZero() {
		pos.LastFundingRatePrefixSum = d.FundingRatePrefixSum
		return k.accountKeeper.SetPosition(ctx, pos)
	}
	// Funding payment = position * delta_prefix_sum / FUNDING_RATE_TICK.
	// position is signed; delta is signed.
	pay := pos.Position.Mul(delta).Quo(math.NewInt(perptypes.FundingRateTick))
	// Adjust accumulated entry quote to reflect funding paid/received.
	pos.EntryQuote = pos.EntryQuote.Add(pay)
	pos.LastFundingRatePrefixSum = d.FundingRatePrefixSum
	return k.accountKeeper.SetPosition(ctx, pos)
}
