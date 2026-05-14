// grpc_query_test.go pins the contracts of the two list query
// handlers: `Orders` honours account/market filters and pagination,
// and `OrderBookSnapshot` limits the iteration to the requested market
// AND caps the depth.
package tests

import (
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/cosmos/cosmos-sdk/types/query"

	perptypes "github.com/perpdex/perpdex-l1/types"
	orderbookkeeper "github.com/perpdex/perpdex-l1/x/orderbook/keeper"
	"github.com/perpdex/perpdex-l1/x/orderbook/types"
)

// TestQueryOrders_HonorsAccountAndMarketFilters confirms the handler
// applies its request parameters: filtering by account_index alone, by
// market_index alone, and by both at the same time yields the matching
// subset of the persisted orders.
func TestQueryOrders_HonorsAccountAndMarketFilters(t *testing.T) {
	k, ctx := newOrderbookKeeper(t)
	q := orderbookkeeper.NewQuerier(k)

	// (account, market) tuples for four orders.
	combos := []struct {
		idx     uint64
		account uint64
		market  uint32
	}{
		{1, 7, 1},
		{2, 7, 2},
		{3, 8, 1},
		{4, 8, 2},
	}
	for _, c := range combos {
		o := makeOrder(c.idx, c.account, c.market, c.idx, false)
		require.NoError(t, k.OpenOrder(ctx, o))
	}

	// account=7 only: orders 1 and 2.
	resp, err := q.Orders(ctx, &types.QueryOrdersRequest{AccountIndex: 7})
	require.NoError(t, err)
	require.Len(t, resp.Orders, 2)
	require.ElementsMatch(t, []uint64{1, 2}, orderIndexes(resp.Orders))

	// market=1 only: orders 1 and 3.
	resp, err = q.Orders(ctx, &types.QueryOrdersRequest{MarketIndex: 1})
	require.NoError(t, err)
	require.ElementsMatch(t, []uint64{1, 3}, orderIndexes(resp.Orders))

	// account=8 and market=1: only order 3.
	resp, err = q.Orders(ctx, &types.QueryOrdersRequest{AccountIndex: 8, MarketIndex: 1})
	require.NoError(t, err)
	require.ElementsMatch(t, []uint64{3}, orderIndexes(resp.Orders))
}

// TestQueryOrders_PaginationLimitsResponse confirms the pagination
// envelope is respected — the handler must not flat-iterate the whole
// Orders table on every call.
func TestQueryOrders_PaginationLimitsResponse(t *testing.T) {
	k, ctx := newOrderbookKeeper(t)
	q := orderbookkeeper.NewQuerier(k)

	for i := uint64(1); i <= 10; i++ {
		o := makeOrder(i, 1, 1, i, false)
		require.NoError(t, k.OpenOrder(ctx, o))
	}

	resp, err := q.Orders(ctx, &types.QueryOrdersRequest{
		Pagination: &query.PageRequest{Limit: 3},
	})
	require.NoError(t, err)
	require.Len(t, resp.Orders, 3, "limit=3 must return three orders, not all ten")
	require.NotNil(t, resp.Pagination, "pagination envelope must be populated")
}

// TestQueryOrderBookSnapshot_DepthAndMarketPrefix exercises the
// snapshot query: it must clamp depth to the configured ceiling per
// side, return bids descending / asks ascending, and not leak across
// markets.
func TestQueryOrderBookSnapshot_DepthAndMarketPrefix(t *testing.T) {
	k, ctx := newOrderbookKeeper(t)
	q := orderbookkeeper.NewQuerier(k)

	mkLimit := func(idx uint64, market uint32, price uint32, isAsk bool) types.Order {
		o := makeOrder(idx, 7, market, idx, isAsk)
		o.Price = price
		o.OrderType = perptypes.LimitOrder
		o.TimeInForce = perptypes.GTT
		return o
	}

	// Market 1: three bids @ 100/101/102, two asks @ 110/111.
	for i, price := range []uint32{100, 101, 102} {
		require.NoError(t, k.OpenOrder(ctx, mkLimit(uint64(i+1), 1, price, false)))
	}
	for i, price := range []uint32{110, 111} {
		require.NoError(t, k.OpenOrder(ctx, mkLimit(uint64(10+i), 1, price, true)))
	}
	// Market 2: one bid that must NOT bleed into the market=1 query.
	require.NoError(t, k.OpenOrder(ctx, mkLimit(99, 2, 500, false)))

	// depth=2 must clip both sides; bids descending, asks ascending.
	resp, err := q.OrderBookSnapshot(ctx, &types.QueryOrderBookSnapshotRequest{MarketIndex: 1, Depth: 2})
	require.NoError(t, err)
	require.Len(t, resp.Bids, 2, "depth=2 must return two bid levels")
	require.Len(t, resp.Asks, 2, "depth=2 must return two ask levels")
	require.Equal(t, []uint32{102, 101}, []uint32{resp.Bids[0].Price, resp.Bids[1].Price},
		"bids must be sorted high → low")
	require.Equal(t, []uint32{110, 111}, []uint32{resp.Asks[0].Price, resp.Asks[1].Price},
		"asks must be sorted low → high")
	for _, lv := range resp.Bids {
		require.Equal(t, uint32(1), lv.MarketIndex)
		require.Positive(t, lv.BidBaseSum, "bid entry must have non-zero bid base")
	}
	for _, lv := range resp.Asks {
		require.Equal(t, uint32(1), lv.MarketIndex)
		require.Positive(t, lv.AskBaseSum, "ask entry must have non-zero ask base")
	}

	// depth=0 must clamp to the keeper-side cap, still scoped to market 1.
	resp, err = q.OrderBookSnapshot(ctx, &types.QueryOrderBookSnapshotRequest{MarketIndex: 1})
	require.NoError(t, err)
	require.LessOrEqual(t, uint32(len(resp.Bids)), types.DefaultOrderBookSnapshotMaxDepth,
		"default depth must clamp bids")
	require.LessOrEqual(t, uint32(len(resp.Asks)), types.DefaultOrderBookSnapshotMaxDepth,
		"default depth must clamp asks")
	for _, lv := range resp.Bids {
		require.Equal(t, uint32(1), lv.MarketIndex)
	}
	for _, lv := range resp.Asks {
		require.Equal(t, uint32(1), lv.MarketIndex)
	}
}

// TestQueryOrders_FilteredPaginationSweep pins the
// `CollectionFilteredPaginate` contract: with a high-miss filter
// (only 2 of 100 orders match) and a small Limit, a paginated sweep
// must surface BOTH matches in at most ⌈matches/Limit⌉ pages — not
// the ~⌈candidates/Limit⌉ pages the old "paginate-then-filter"
// implementation required.
func TestQueryOrders_FilteredPaginationSweep(t *testing.T) {
	k, ctx := newOrderbookKeeper(t)
	q := orderbookkeeper.NewQuerier(k)

	// 100 orders for account=9; only #50 and #100 are for account=7.
	for i := uint64(1); i <= 100; i++ {
		acc := uint64(9)
		if i == 50 || i == 100 {
			acc = 7
		}
		o := makeOrder(i, acc, 1, i, false)
		require.NoError(t, k.OpenOrder(ctx, o))
	}

	var got []uint64
	var nextKey []byte
	steps := 0
	for {
		resp, err := q.Orders(ctx, &types.QueryOrdersRequest{
			AccountIndex: 7,
			Pagination:   &query.PageRequest{Limit: 3, Key: nextKey},
		})
		require.NoError(t, err)
		for _, o := range resp.Orders {
			got = append(got, o.OrderIndex)
		}
		steps++
		if resp.Pagination == nil || len(resp.Pagination.NextKey) == 0 {
			break
		}
		nextKey = resp.Pagination.NextKey
		require.Less(t, steps, 50, "sweep must converge; this guards against an O(N) page-explosion regression")
	}
	require.ElementsMatch(t, []uint64{50, 100}, got,
		"filter=account 7 must surface both matches via paginated sweep")
	// Two matches, Limit=3 → the entire sweep fits in a single page.
	require.LessOrEqual(t, steps, 2,
		"CollectionFilteredPaginate must finish in ≤2 pages for 2 matches with Limit=3")
}

func orderIndexes(orders []types.Order) []uint64 {
	out := make([]uint64, 0, len(orders))
	for _, o := range orders {
		out = append(out, o.OrderIndex)
	}
	return out
}
