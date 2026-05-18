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

// SettlePositionFunding applies the per-round funding payment to a
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
// which matches the `funding = position * mark * fundingRate` formula.
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
//
// Short-circuits when there is no open position (issue #91):
// AccountPositions only carries open positions or leverage-only config
// rows (BaseSize == 0). A closed / never-opened account has no funding
// obligation, and the next open via x/trade is responsible for seeding
// `LastFundingRatePrefixSum` from the market's current value, so we
// don't need to maintain the snapshot on empty rows here.
func (k Keeper) SettlePositionFunding(ctx context.Context, accountIndex uint64, marketIndex uint32) error {
	pos, err := k.accountKeeper.GetPosition(ctx, accountIndex, marketIndex)
	if err != nil {
		return err
	}
	if pos.BaseSize.IsZero() {
		return nil
	}
	d, err := k.marketKeeper.GetMarketDetails(ctx, marketIndex)
	if err != nil {
		return err
	}
	delta := d.FundingRatePrefixSum.Sub(pos.LastFundingRatePrefixSum)
	if delta.IsZero() {
		return nil
	}
	_, err = k.accountKeeper.MutatePosition(ctx, accountIndex, marketIndex, func(pos *accounttypes.AccountPosition) error {
		pay := pos.BaseSize.Mul(delta).Quo(math.NewInt(perptypes.FundingRateTick))
		pos.EntryQuote = pos.EntryQuote.Add(pay)
		pos.LastFundingRatePrefixSum = d.FundingRatePrefixSum
		return nil
	})
	return err
}
