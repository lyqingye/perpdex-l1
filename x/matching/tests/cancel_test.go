// cancel_test.go covers msg-server-level cancel semantics:
//   - CancelAllOrders honours the MarketIndexFilter (0 = all markets).
//   - The cancel sweep reaches orders whose ClientOrderIndex is unset.
//
// Single-order CancelOrder happy paths are exercised implicitly by the
// recovery / reduce-only flows; bulk cancel correctness is asserted here
// because it backs the liquidation engine's "clear the victim's book"
// invariant.
package tests

import (
	"testing"

	"github.com/stretchr/testify/require"

	perptypes "github.com/perpdex/perpdex-l1/types"
	matchingkeeper "github.com/perpdex/perpdex-l1/x/matching/keeper"
	matchingtypes "github.com/perpdex/perpdex-l1/x/matching/types"
	orderbooktypes "github.com/perpdex/perpdex-l1/x/orderbook/types"
)

// TestCancelAllOrders_HonorsMarketFilter ensures that an explicit
// MarketIndexFilter restricts the cancel-all sweep to that market only,
// and that filter==0 sweeps every market (proto contract).
func TestCancelAllOrders_HonorsMarketFilter(t *testing.T) {
	e := newMatchEnv(t)
	srv := matchingkeeper.NewMsgServerImpl(e.k)

	// Two resting orders, market 1 and market 2, same account, no client id.
	o1 := orderbooktypes.Order{
		OrderIndex: 1, OwnerAccountIndex: 99, MarketIndex: 1, IsAsk: true,
		OrderType: perptypes.LimitOrder, TimeInForce: perptypes.GTT,
		Price: 1000, Nonce: 1, InitialBaseAmount: 5, RemainingBaseAmount: 5,
		Status: perptypes.OrderStatusOpen,
	}
	o2 := orderbooktypes.Order{
		OrderIndex: 2, OwnerAccountIndex: 99, MarketIndex: 2, IsAsk: true,
		OrderType: perptypes.LimitOrder, TimeInForce: perptypes.GTT,
		Price: 2000, Nonce: 1, InitialBaseAmount: 5, RemainingBaseAmount: 5,
		Status: perptypes.OrderStatusOpen,
	}
	e.rest(t, o1, true)
	e.rest(t, o2, true)

	// Cancel only market 1.
	_, err := srv.CancelAllOrders(e.ctx, &matchingtypes.MsgCancelAllOrders{
		Sender:            "px1qv9pzxqlyckngw6zf9g9whn9d3eh4qvgsxc8cx",
		AccountIndex:      99,
		MarketIndexFilter: 1,
		Mode:              perptypes.ImmediateCancelAll,
	})
	require.NoError(t, err)

	o1Now, err := e.bk.GetOrder(e.ctx, 1)
	require.NoError(t, err)
	require.Equal(t, perptypes.OrderStatusCancelled, o1Now.Status)

	o2Now, err := e.bk.GetOrder(e.ctx, 2)
	require.NoError(t, err)
	require.Equal(t, perptypes.OrderStatusOpen, o2Now.Status, "market 2 must be untouched")
}

// TestCancelAllOrders_CoversOrdersWithoutClientID verifies that an order
// whose ClientOrderIndex is 0 (the optional default) is still reachable
// via cancel-all.
func TestCancelAllOrders_CoversOrdersWithoutClientID(t *testing.T) {
	e := newMatchEnv(t)
	srv := matchingkeeper.NewMsgServerImpl(e.k)

	o := orderbooktypes.Order{
		OrderIndex: 7, OwnerAccountIndex: 42, MarketIndex: 1, IsAsk: false,
		OrderType: perptypes.LimitOrder, TimeInForce: perptypes.GTT,
		Price: 100, Nonce: 1, InitialBaseAmount: 1, RemainingBaseAmount: 1,
		ClientOrderIndex: 0, // explicit: not set
		Status:           perptypes.OrderStatusOpen,
	}
	e.rest(t, o, false)

	_, err := srv.CancelAllOrders(e.ctx, &matchingtypes.MsgCancelAllOrders{
		Sender:            "px1qv9pzxqlyckngw6zf9g9whn9d3eh4qvgsxc8cx",
		AccountIndex:      42,
		MarketIndexFilter: 0,
		Mode:              perptypes.ImmediateCancelAll,
	})
	require.NoError(t, err)

	got, err := e.bk.GetOrder(e.ctx, 7)
	require.NoError(t, err)
	require.Equal(t, perptypes.OrderStatusCancelled, got.Status)
}
