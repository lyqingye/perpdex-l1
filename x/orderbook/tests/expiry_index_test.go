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

	"cosmossdk.io/collections"

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

// requireExpiryIndex asserts the (expiry, orderIndex) entry has the
// expected presence in the ExpiryIndex keyset. The helper exists so
// the trigger-activation tests can pin every ExpiryIndex transition
// without re-typing the join boilerplate.
func requireExpiryIndex(t *testing.T, k orderbookkeeper.Keeper, ctx sdk.Context, expiry int64, orderIndex uint64, want bool) {
	t.Helper()
	has, err := k.ExpiryIndex.Has(ctx, collections.Join(expiry, orderIndex))
	require.NoError(t, err)
	require.Equal(t, want, has,
		"ExpiryIndex(%d, %d) presence mismatch: want=%v", expiry, orderIndex, want)
}

// TestExpiry_TriggerActivatedGTTRestsAndExpires pins the
// ActivateTrigger → OpenOrder → EndBlocker rotation: the
// pre-activation ExpiryIndex entry must be dropped by
// `ActivateTrigger`, the post-match `OpenOrder` reinstates it for the
// activated GTT residual, and EndBlocker still cancels at expiry.
// This locks finding 1-C against regression.
func TestExpiry_TriggerActivatedGTTRestsAndExpires(t *testing.T) {
	k, ctx := newOrderbookKeeper(t)

	o := makeOrder(1, 77, 1, 10, false)
	o.Status = perptypes.OrderStatusTriggeredPending
	o.OrderType = perptypes.StopLossLimitOrder
	o.TriggerPrice = 999
	o.TimeInForce = perptypes.GTT
	o.Expiry = 400
	require.NoError(t, k.OpenTriggerOrder(ctx, o))
	requireExpiryIndex(t, k, ctx, 400, 1, true)

	activated, err := k.ActivateTrigger(ctx, 1)
	require.NoError(t, err)
	requireExpiryIndex(t, k, ctx, 400, 1, false)

	// Mimic the matching keeper: trigger-limit becomes a plain
	// limit, TIF preserved (still GTT). Re-rest the activated
	// residual on the book via OpenOrder.
	activated.OrderType = perptypes.LimitOrder
	require.NoError(t, k.OpenOrder(ctx, activated))
	requireExpiryIndex(t, k, ctx, 400, 1, true)

	ctxAt := sdk.UnwrapSDKContext(ctx).WithBlockHeader(cmtprototypes.Header{Time: time.UnixMilli(500)})
	require.NoError(t, k.EndBlocker(ctxAt))
	requireStatus(t, k, ctx, 1, perptypes.OrderStatusCancelled)
	requireExpiryIndex(t, k, ctx, 400, 1, false)
}

// TestExpiry_TriggerActivatedTerminalDropsIndex covers the IOC /
// fully-filled branch: a stop-market order activates, matches, and
// the post-match OpenOrder hits the terminal branch — the pre-
// activation ExpiryIndex entry must be gone (ActivateTrigger cleared
// it) and OpenOrder must also no-op on its own re-clear path.
func TestExpiry_TriggerActivatedTerminalDropsIndex(t *testing.T) {
	k, ctx := newOrderbookKeeper(t)

	o := makeOrder(1, 77, 1, 10, false)
	o.Status = perptypes.OrderStatusTriggeredPending
	o.OrderType = perptypes.StopLossOrder
	o.TriggerPrice = 999
	o.TimeInForce = perptypes.GTT
	o.Expiry = 400
	require.NoError(t, k.OpenTriggerOrder(ctx, o))
	requireExpiryIndex(t, k, ctx, 400, 1, true)

	activated, err := k.ActivateTrigger(ctx, 1)
	require.NoError(t, err)
	requireExpiryIndex(t, k, ctx, 400, 1, false)

	// Mimic matching: stop-market becomes a market+IOC; the entire
	// residual fills synchronously, so the post-match OpenOrder hits
	// the terminal branch.
	activated.OrderType = perptypes.MarketOrder
	activated.TimeInForce = perptypes.IOC
	activated.Price = 0
	activated.RemainingBaseAmount = 0
	activated.Status = perptypes.OrderStatusFilled
	require.NoError(t, k.OpenOrder(ctx, activated))
	requireExpiryIndex(t, k, ctx, 400, 1, false)

	// EndBlocker at a future timestamp must not try to re-cancel
	// the now-filled order; the keyset is already empty.
	ctxAt := sdk.UnwrapSDKContext(ctx).WithBlockHeader(cmtprototypes.Header{Time: time.UnixMilli(500)})
	require.NoError(t, k.EndBlocker(ctxAt))
	requireStatus(t, k, ctx, 1, perptypes.OrderStatusFilled)
}

// TestExpiry_TriggerActivatedThenCancelledClearsIndex models the
// matching-error path: ActivateTrigger succeeds but the subsequent
// match aborts, so the caller CancelOrders the just-activated order.
// CancelOrder must still leave the keyset clean.
func TestExpiry_TriggerActivatedThenCancelledClearsIndex(t *testing.T) {
	k, ctx := newOrderbookKeeper(t)

	o := makeOrder(1, 77, 1, 10, false)
	o.Status = perptypes.OrderStatusTriggeredPending
	o.OrderType = perptypes.StopLossLimitOrder
	o.TriggerPrice = 999
	o.TimeInForce = perptypes.GTT
	o.Expiry = 400
	require.NoError(t, k.OpenTriggerOrder(ctx, o))

	_, err := k.ActivateTrigger(ctx, 1)
	require.NoError(t, err)
	requireExpiryIndex(t, k, ctx, 400, 1, false)

	// Cancel the just-activated order before any match touches it.
	// CancelOrder hits the Open branch via removeOrderbookEntry's
	// ErrNotFound tolerance and then no-ops on removeExpiryIndex
	// (already cleared).
	_, err = k.CancelOrder(ctx, 1)
	require.NoError(t, err)
	requireStatus(t, k, ctx, 1, perptypes.OrderStatusCancelled)
	requireExpiryIndex(t, k, ctx, 400, 1, false)
}
