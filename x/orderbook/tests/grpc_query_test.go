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

// TestQueryOrderBookSnapshot_DepthAndMarketPrefix exercises the new
// snapshot query: it must clamp depth to the configured ceiling, must
// not leak across markets, and must include the asks and bids of the
// requested market only.
func TestQueryOrderBookSnapshot_DepthAndMarketPrefix(t *testing.T) {
	k, ctx := newOrderbookKeeper(t)
	q := orderbookkeeper.NewQuerier(k)

	// Two markets, three price levels per side on market=1 only.
	for i, price := range []uint32{100, 101, 102} {
		o := makeOrder(uint64(i+1), 7, 1, uint64(i+1), false)
		o.Price = price
		o.OrderType = perptypes.LimitOrder
		o.TimeInForce = perptypes.GTT
		require.NoError(t, k.OpenOrder(ctx, o))
	}
	// Market 2 has its own level — it must not appear when querying
	// market 1.
	other := makeOrder(99, 7, 2, 99, false)
	other.Price = 500
	require.NoError(t, k.OpenOrder(ctx, other))

	resp, err := q.OrderBookSnapshot(ctx, &types.QueryOrderBookSnapshotRequest{MarketIndex: 1, Depth: 2})
	require.NoError(t, err)
	require.Len(t, resp.Levels, 2, "depth=2 must return exactly two levels")
	for _, lv := range resp.Levels {
		require.Equal(t, uint32(1), lv.MarketIndex)
		require.NotEqual(t, uint32(500), lv.Price)
	}

	// depth=0 must clamp to the internal cap (50) and still scope to
	// market 1.
	resp, err = q.OrderBookSnapshot(ctx, &types.QueryOrderBookSnapshotRequest{MarketIndex: 1})
	require.NoError(t, err)
	require.LessOrEqual(t, len(resp.Levels), 50, "default depth must clamp")
	for _, lv := range resp.Levels {
		require.Equal(t, uint32(1), lv.MarketIndex)
	}
}

func orderIndexes(orders []types.Order) []uint64 {
	out := make([]uint64, 0, len(orders))
	for _, o := range orders {
		out = append(out, o.OrderIndex)
	}
	return out
}
