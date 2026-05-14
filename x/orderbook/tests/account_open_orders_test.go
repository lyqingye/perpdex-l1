// account_open_orders_test.go covers the AccountOpenOrders KeySet that
// powers cancel-all: orders without a client_order_index must still be
// reachable, the per-market filter must restrict iteration, and a
// CancelOrder transition must drop the order from the iterator.
package tests

import (
	"testing"

	"github.com/stretchr/testify/require"

	perptypes "github.com/perpdex/perpdex-l1/types"
	"github.com/perpdex/perpdex-l1/x/orderbook/types"
)

// TestAccountOpenOrders_CoversNoClientID confirms that orders without a
// client_order_index (the optional field defaulting to 0) are still
// reachable via the AccountOpenOrders index, so cancel-all can find them.
func TestAccountOpenOrders_CoversNoClientID(t *testing.T) {
	k, ctx := newOrderbookKeeper(t)

	a := makeOrder(1, 99, 1, 10, false)
	require.NoError(t, k.OpenOrder(ctx, a))

	b := makeOrder(2, 99, 1, 0, false)
	require.NoError(t, k.OpenOrder(ctx, b))

	seen := map[uint64]bool{}
	require.NoError(t, k.IterateAccountOpenOrders(ctx, 99, 0, func(o types.Order) error {
		seen[o.OrderIndex] = true
		return nil
	}))
	require.True(t, seen[1], "order with client_order_index missed")
	require.True(t, seen[2], "order without client_order_index missed")
}

// TestAccountOpenOrders_HonorsMarketFilter checks that a non-zero
// market filter restricts iteration to that market only.
func TestAccountOpenOrders_HonorsMarketFilter(t *testing.T) {
	k, ctx := newOrderbookKeeper(t)

	for i, mkt := range []uint32{1, 2, 1, 3} {
		o := makeOrder(uint64(i+1), 7, mkt, 0, false)
		require.NoError(t, k.OpenOrder(ctx, o))
	}

	collect := func(filter uint32) []uint64 {
		var got []uint64
		require.NoError(t, k.IterateAccountOpenOrders(ctx, 7, filter, func(o types.Order) error {
			got = append(got, o.OrderIndex)
			return nil
		}))
		return got
	}

	require.ElementsMatch(t, []uint64{1, 2, 3, 4}, collect(0), "filter=0 should yield all markets")
	require.ElementsMatch(t, []uint64{1, 3}, collect(1), "filter=1 should yield market 1 only")
	require.ElementsMatch(t, []uint64{2}, collect(2), "filter=2 should yield market 2 only")
	require.Empty(t, collect(99), "filter for non-existent market should yield nothing")
}

// TestAccountOpenOrders_CancelRemoves verifies that CancelOrder removes
// the order from the AccountOpenOrders iterator.
func TestAccountOpenOrders_CancelRemoves(t *testing.T) {
	k, ctx := newOrderbookKeeper(t)

	o := makeOrder(5, 1, 1, 0, false)
	require.NoError(t, k.OpenOrder(ctx, o))

	var hits int
	require.NoError(t, k.IterateAccountOpenOrders(ctx, 1, 0, func(types.Order) error {
		hits++
		return nil
	}))
	require.Equal(t, 1, hits)

	cancelled, err := k.CancelOrder(ctx, o.OrderIndex)
	require.NoError(t, err)
	require.Equal(t, perptypes.OrderStatusCancelled, cancelled.Status)

	hits = 0
	require.NoError(t, k.IterateAccountOpenOrders(ctx, 1, 0, func(types.Order) error {
		hits++
		return nil
	}))
	require.Zero(t, hits)
}
