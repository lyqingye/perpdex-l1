package keeper

import (
	"context"
	"fmt"

	"cosmossdk.io/collections"
	"cosmossdk.io/core/store"
	"cosmossdk.io/math"

	"github.com/cosmos/cosmos-sdk/codec"

	perptypes "github.com/perpdex/perpdex-l1/types"
	accounttypes "github.com/perpdex/perpdex-l1/x/account/types"
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

// SettlePositionFunding applies the per-round Lighter funding payment to a
// position by leveraging the cumulative prefix sum maintained by
// `settleMarket`.
//
// The prefix sum stores `Σ mark_t * rate_t` across rounds, so taking the
// delta against the position's snapshot and multiplying by the (signed)
// position size gives:
//
//	pay = position * (Σ_now mark_t*rate_t - Σ_last mark_t*rate_t) / FundingRateTick
//	    = Σ_unsettled (position * mark_t * rate_t) / FundingRateTick
//
// which matches Lighter's `funding = position * mark * fundingRate`.
//
// The funding amount is applied to `EntryQuote` so it folds directly into
// `uPnL = position * mark - EntryQuote`:
//   - long with positive funding rate: `pay > 0`, `EntryQuote` rises and
//     `uPnL` drops by exactly the funding the long paid out.
//   - short with positive funding rate: `pay < 0`, `EntryQuote` falls and
//     `uPnL` rises by exactly the funding the short received.
//
// Returns nil on success and snapshots the new prefix sum on the position so
// the next settlement only charges newly accumulated rounds.
func (k Keeper) SettlePositionFunding(ctx context.Context, accountIndex uint64, marketIndex uint32) error {
	d, err := k.marketKeeper.GetMarketDetails(ctx, marketIndex)
	if err != nil {
		return err
	}
	if d.FundingRatePrefixSum.IsNil() {
		d.FundingRatePrefixSum = math.ZeroInt()
	}
	_, err = k.accountKeeper.UpdatePosition(ctx, accountIndex, marketIndex, func(pos *accounttypes.AccountPosition) error {
		if pos.LastFundingRatePrefixSum.IsNil() {
			pos.LastFundingRatePrefixSum = math.ZeroInt()
		}
		if pos.Position.IsNil() {
			pos.Position = math.ZeroInt()
		}
		if pos.EntryQuote.IsNil() {
			pos.EntryQuote = math.ZeroInt()
		}
		if pos.Position.IsZero() {
			pos.LastFundingRatePrefixSum = d.FundingRatePrefixSum
			return nil
		}
		delta := d.FundingRatePrefixSum.Sub(pos.LastFundingRatePrefixSum)
		if delta.IsZero() {
			pos.LastFundingRatePrefixSum = d.FundingRatePrefixSum
			return nil
		}
		pay := pos.Position.Mul(delta).Quo(math.NewInt(perptypes.FundingRateTick))
		pos.EntryQuote = pos.EntryQuote.Add(pay)
		pos.LastFundingRatePrefixSum = d.FundingRatePrefixSum
		return nil
	})
	return err
}
