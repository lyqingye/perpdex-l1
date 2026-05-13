// reduce_only_test.go covers the matching kernel's reduce-only
// invariants on the MAKER side (taker reduce-only is exercised
// implicitly by the liquidation flow). Two complementary cases:
//
//   - A reduce-only maker that no longer holds an opposite-side
//     position must be evicted, AND the eviction must clear every
//     index that exposes the order (status, client-id mapping,
//     account-open iterator).
//   - A reduce-only maker that DOES hold a position must cap the
//     fill against |position| so a single trade cannot flip the
//     maker to the opposite side.
package tests

import (
	"testing"

	"github.com/stretchr/testify/require"

	perptypes "github.com/perpdex/perpdex-l1/types"
	orderbooktypes "github.com/perpdex/perpdex-l1/x/orderbook/types"
)

// TestMatchOrder_EvictReduceOnlyClearsOrderRecord pins the invariant
// that evicting a reduce-only maker (no opposite-direction position)
// goes through EvictMakerOrder, which atomically removes the entry,
// marks the Order Cancelled, and clears the client + account-open
// indexes — so a stale "open" order cannot linger after eviction.
func TestMatchOrder_EvictReduceOnlyClearsOrderRecord(t *testing.T) {
	e := newMatchEnv(t)

	// maker account 10 holds no position, so the reduce-only ask is
	// invalid the moment the taker bids against it.
	maker := orderbooktypes.Order{
		OrderIndex:          1,
		ClientOrderIndex:    7,
		OwnerAccountIndex:   10,
		MarketIndex:         1,
		IsAsk:               true,
		OrderType:           perptypes.LimitOrder,
		TimeInForce:         perptypes.GTT,
		Price:               1000,
		Nonce:               1,
		InitialBaseAmount:   5,
		RemainingBaseAmount: 5,
		ReduceOnly:          true,
		Status:              perptypes.OrderStatusOpen,
	}
	e.rest(t, maker, true)

	// Sanity: the AccountOpenOrders index sees the maker as resting.
	var pre int
	require.NoError(t, e.bk.IterateAccountOpenOrders(e.ctx, 10, 0, func(orderbooktypes.Order) bool {
		pre++
		return false
	}))
	require.Equal(t, 1, pre)

	taker := &orderbooktypes.Order{
		OrderIndex:          2,
		OwnerAccountIndex:   20,
		MarketIndex:         1,
		IsAsk:               false,
		OrderType:           perptypes.LimitOrder,
		TimeInForce:         perptypes.GTT,
		Price:               1000,
		Nonce:               2,
		InitialBaseAmount:   5,
		RemainingBaseAmount: 5,
		Status:              perptypes.OrderStatusOpen,
	}

	filled, _, err := e.k.MatchOrder(e.ctx, taker, 16)
	require.NoError(t, err)
	require.Zero(t, filled, "reduce-only maker without position must not produce a fill")
	require.Empty(t, e.tk.fills)

	got, err := e.bk.GetOrder(e.ctx, 1)
	require.NoError(t, err)
	require.Equal(t, perptypes.OrderStatusCancelled, got.Status)

	// Client + account-open indexes are cleared.
	_, err = e.bk.GetOrderByClientID(e.ctx, 1, 10, 7)
	require.Error(t, err, "client_order_index mapping should be removed after eviction")

	var post int
	require.NoError(t, e.bk.IterateAccountOpenOrders(e.ctx, 10, 0, func(orderbooktypes.Order) bool {
		post++
		return false
	}))
	require.Zero(t, post, "evicted reduce-only maker must not survive in AccountOpenOrders")
}

// TestMatchOrder_MakerReduceOnlyNoFlip enforces that a reduce-only maker
// cannot flip its own position even if the taker requests more base than
// the maker actually holds. With maker long=5 and taker bid 10 against a
// reduce-only ask of size 10, only 5 may fill.
func TestMatchOrder_MakerReduceOnlyNoFlip(t *testing.T) {
	e := newMatchEnv(t)

	// maker is long 5
	e.ak.setPosition(10, 1, 5)

	maker := orderbooktypes.Order{
		OrderIndex:          1,
		OwnerAccountIndex:   10,
		MarketIndex:         1,
		IsAsk:               true,
		OrderType:           perptypes.LimitOrder,
		TimeInForce:         perptypes.GTT,
		Price:               1000,
		Nonce:               1,
		InitialBaseAmount:   10,
		RemainingBaseAmount: 10,
		ReduceOnly:          true,
		Status:              perptypes.OrderStatusOpen,
	}
	e.rest(t, maker, true)

	taker := &orderbooktypes.Order{
		OrderIndex:          2,
		OwnerAccountIndex:   20,
		MarketIndex:         1,
		IsAsk:               false,
		OrderType:           perptypes.LimitOrder,
		TimeInForce:         perptypes.GTT,
		Price:               1000,
		Nonce:               2,
		InitialBaseAmount:   10,
		RemainingBaseAmount: 10,
		Status:              perptypes.OrderStatusOpen,
	}

	filled, _, err := e.k.MatchOrder(e.ctx, taker, 16)
	require.NoError(t, err)
	require.EqualValues(t, 5, filled, "maker reduce-only must cap fill to maker's |position|")
	require.Len(t, e.tk.fills, 1)
	require.EqualValues(t, 5, e.tk.fills[0].BaseAmount)
}
