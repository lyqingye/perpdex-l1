// expiry_index_test.go covers the GTT ExpiryIndex powering the
// EndBlocker: each resting / trigger-pending GTT order with a non-zero
// expiry is registered in the keyset on OpenOrder /
// OpenTriggerOrder, and torn down again on CancelOrder /
// FillMakerOrder / EvictMakerOrder. EndBlocker walks the keyset in
// ascending expiry order and stops at the first still-future entry,
// so each block does O(due_orders) work instead of O(Orders_history).
package tests

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	cmtprototypes "github.com/cometbft/cometbft/proto/tendermint/types"

	sdk "github.com/cosmos/cosmos-sdk/types"

	perptypes "github.com/perpdex/perpdex-l1/types"
	orderbookkeeper "github.com/perpdex/perpdex-l1/x/orderbook/keeper"
	"github.com/perpdex/perpdex-l1/x/orderbook/types"
)

// TestExpiry_EndBlockerCancelsOnlyDueOrders builds a book with three
// GTT orders staggered in time and one non-GTT POSTONLY order, then
// drives EndBlocker at two intermediate timestamps. The non-GTT order
// must survive both passes (it is never registered in the expiry
// keyset); each GTT order must drop out exactly once its expiry is
// reached.
func TestExpiry_EndBlockerCancelsOnlyDueOrders(t *testing.T) {
	k, ctx := newOrderbookKeeper(t)

	mkGTT := func(idx uint64, expiry int64) types.Order {
		o := makeOrder(idx, 77, 1, idx, false)
		o.TimeInForce = perptypes.GTT
		o.Expiry = expiry
		return o
	}
	require.NoError(t, k.OpenOrder(ctx, mkGTT(1, 100)))
	require.NoError(t, k.OpenOrder(ctx, mkGTT(2, 200)))
	require.NoError(t, k.OpenOrder(ctx, mkGTT(3, 300)))

	postOnly := makeOrder(4, 77, 1, 99, true)
	postOnly.TimeInForce = perptypes.PostOnly
	postOnly.Expiry = 0
	require.NoError(t, k.OpenOrder(ctx, postOnly))

	// Tick to t = 150ms: only order 1 is due.
	ctxAt := sdk.UnwrapSDKContext(ctx).WithBlockHeader(cmtprototypes.Header{Time: time.UnixMilli(150)})
	require.NoError(t, k.EndBlocker(ctxAt))

	requireStatus(t, k, ctx, 1, perptypes.OrderStatusCancelled)
	requireStatus(t, k, ctx, 2, perptypes.OrderStatusOpen)
	requireStatus(t, k, ctx, 3, perptypes.OrderStatusOpen)
	requireStatus(t, k, ctx, 4, perptypes.OrderStatusOpen)

	// Tick to t = 250ms: order 2 expires; order 3 still in future.
	ctxAt = sdk.UnwrapSDKContext(ctx).WithBlockHeader(cmtprototypes.Header{Time: time.UnixMilli(250)})
	require.NoError(t, k.EndBlocker(ctxAt))

	requireStatus(t, k, ctx, 2, perptypes.OrderStatusCancelled)
	requireStatus(t, k, ctx, 3, perptypes.OrderStatusOpen)
	requireStatus(t, k, ctx, 4, perptypes.OrderStatusOpen)
}

// TestExpiry_FillRemovesIndex confirms a maker that fully fills drops
// out of the expiry keyset, so a later EndBlocker pass past the
// original expiry timestamp does not attempt to re-cancel a
// terminal-status order.
func TestExpiry_FillRemovesIndex(t *testing.T) {
	k, ctx := newOrderbookKeeper(t)

	o := makeOrder(1, 77, 1, 10, false)
	o.TimeInForce = perptypes.GTT
	o.Expiry = 200
	require.NoError(t, k.OpenOrder(ctx, o))

	// Fully fill the maker; it must leave the expiry keyset.
	_, err := k.FillMakerOrder(ctx, 1, o.RemainingBaseAmount)
	require.NoError(t, err)

	ctxAt := sdk.UnwrapSDKContext(ctx).WithBlockHeader(cmtprototypes.Header{Time: time.UnixMilli(500)})
	require.NoError(t, k.EndBlocker(ctxAt), "EndBlocker must tolerate a filled GTT order whose entry has already left the index")

	requireStatus(t, k, ctx, 1, perptypes.OrderStatusFilled)
}

// TestExpiry_CancelRemovesIndex makes sure user-initiated cancels drop
// the expiry entry too — otherwise EndBlocker would later try to
// cancel a Cancelled order and surface ErrOrderNotCancelable as a
// non-recoverable error.
func TestExpiry_CancelRemovesIndex(t *testing.T) {
	k, ctx := newOrderbookKeeper(t)

	o := makeOrder(1, 77, 1, 10, false)
	o.TimeInForce = perptypes.GTT
	o.Expiry = 200
	require.NoError(t, k.OpenOrder(ctx, o))

	_, err := k.CancelOrder(ctx, 1)
	require.NoError(t, err)

	ctxAt := sdk.UnwrapSDKContext(ctx).WithBlockHeader(cmtprototypes.Header{Time: time.UnixMilli(500)})
	require.NoError(t, k.EndBlocker(ctxAt))
}

// TestExpiry_TriggerOrderIndexed proves trigger-pending GTT orders are
// also tracked: a stop-loss that expires before the mark crosses its
// trigger price must still be cancelled at expiry by EndBlocker.
func TestExpiry_TriggerOrderIndexed(t *testing.T) {
	k, ctx := newOrderbookKeeper(t)

	o := makeOrder(1, 77, 1, 10, false)
	o.Status = perptypes.OrderStatusTriggeredPending
	o.OrderType = perptypes.StopLossLimitOrder
	o.TriggerPrice = 999
	o.TimeInForce = perptypes.GTT
	o.Expiry = 150
	require.NoError(t, k.OpenTriggerOrder(ctx, o))

	ctxAt := sdk.UnwrapSDKContext(ctx).WithBlockHeader(cmtprototypes.Header{Time: time.UnixMilli(500)})
	require.NoError(t, k.EndBlocker(ctxAt))

	requireStatus(t, k, ctx, 1, perptypes.OrderStatusCancelled)
}

func requireStatus(t *testing.T, k orderbookkeeper.Keeper, ctx sdk.Context, idx uint64, want uint32) {
	t.Helper()
	o, err := k.GetOrder(ctx, idx)
	require.NoError(t, err)
	require.Equal(t, want, o.Status, "order %d status mismatch", idx)
}
