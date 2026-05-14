// genesis_test.go covers `InitGenesis` rehydration: that a chain
// restarting from a snapshot rebuilds every per-order index (client
// order map, account-open-order iterator, side-sorted orderbook, the
// trigger index, and the GTT ExpiryIndex) and that terminal-status
// orders are NOT re-indexed as open.
package tests

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	cmtprototypes "github.com/cometbft/cometbft/proto/tendermint/types"

	"cosmossdk.io/collections"

	sdk "github.com/cosmos/cosmos-sdk/types"

	perptypes "github.com/perpdex/perpdex-l1/types"
	"github.com/perpdex/perpdex-l1/x/orderbook/types"
)

func TestGenesis_RestoresOrderIndexes(t *testing.T) {
	k, ctx := newOrderbookKeeper(t)

	open := makeOrder(1, 99, 1, 10, false)
	trigger := makeOrder(2, 99, 1, 11, true)
	trigger.Status = perptypes.OrderStatusTriggeredPending
	trigger.OrderType = perptypes.StopLossLimitOrder
	trigger.TriggerPrice = 900
	cancelled := makeOrder(3, 99, 1, 12, false)
	cancelled.Status = perptypes.OrderStatusCancelled

	require.NoError(t, k.InitGenesis(ctx, types.GenesisState{
		Params:         types.DefaultParams(),
		NextOrderIndex: 4,
		Orders:         []types.Order{open, trigger, cancelled},
	}))

	foundClient, orderIndex, err := k.HasOpenClientOrder(ctx, open.MarketIndex, open.OwnerAccountIndex, open.ClientOrderIndex)
	require.NoError(t, err)
	require.True(t, foundClient)
	require.Equal(t, open.OrderIndex, orderIndex)

	count, err := k.GetAccountOpenOrderCount(ctx, open.OwnerAccountIndex, open.MarketIndex)
	require.NoError(t, err)
	require.EqualValues(t, 2, count)

	seenOpen := map[uint64]bool{}
	require.NoError(t, k.IterateAccountOpenOrders(ctx, open.OwnerAccountIndex, 0, func(o types.Order) error {
		seenOpen[o.OrderIndex] = true
		return nil
	}))
	require.True(t, seenOpen[open.OrderIndex])
	require.True(t, seenOpen[trigger.OrderIndex])
	require.False(t, seenOpen[cancelled.OrderIndex])

	best, ok, err := k.PeekBest(ctx, open.MarketIndex, false)
	require.NoError(t, err)
	require.True(t, ok)
	require.Equal(t, open.OrderIndex, best.OrderIndex)

	seenTrigger := false
	require.NoError(t, k.IterateTriggers(ctx, func(o types.Order) error {
		if o.MarketIndex == trigger.MarketIndex &&
			o.TriggerPrice == trigger.TriggerPrice &&
			o.OrderIndex == trigger.OrderIndex {
			seenTrigger = true
			return types.ErrStopIteration
		}
		return nil
	}))
	require.True(t, seenTrigger)

	foundCancelled, _, err := k.HasOpenClientOrder(ctx, cancelled.MarketIndex, cancelled.OwnerAccountIndex, cancelled.ClientOrderIndex)
	require.NoError(t, err)
	require.False(t, foundCancelled)
}

// TestGenesis_RestoresExpiryIndex confirms `restoreGenesisOrderIndexes`
// re-installs the GTT ExpiryIndex for both resting Open and parked
// TriggeredPending orders. Without this rehydration, a chain restart
// would silently disable EndBlocker's expiry sweep for every order
// imported from the snapshot — users would lose their GTT protection
// across upgrades.
func TestGenesis_RestoresExpiryIndex(t *testing.T) {
	k, ctx := newOrderbookKeeper(t)

	gttOpen := makeOrder(4, 99, 1, 13, false)
	gttOpen.TimeInForce = perptypes.GTT
	gttOpen.Expiry = 200

	gttTrigger := makeOrder(5, 99, 1, 14, false)
	gttTrigger.Status = perptypes.OrderStatusTriggeredPending
	gttTrigger.OrderType = perptypes.StopLossLimitOrder
	gttTrigger.TriggerPrice = 800
	gttTrigger.TimeInForce = perptypes.GTT
	gttTrigger.Expiry = 300

	// A GTT order WITHOUT a non-zero Expiry must NOT show up in the
	// keyset — the index only carries actually-expiring orders.
	gttNoExpiry := makeOrder(6, 99, 1, 15, true)
	gttNoExpiry.TimeInForce = perptypes.GTT
	gttNoExpiry.Expiry = 0

	require.NoError(t, k.InitGenesis(ctx, types.GenesisState{
		Params:         types.DefaultParams(),
		NextOrderIndex: 7,
		Orders:         []types.Order{gttOpen, gttTrigger, gttNoExpiry},
	}))

	has, err := k.ExpiryIndex.Has(ctx, collections.Join(int64(200), uint64(4)))
	require.NoError(t, err)
	require.True(t, has, "GTT Open order must reinstate ExpiryIndex on genesis")

	has, err = k.ExpiryIndex.Has(ctx, collections.Join(int64(300), uint64(5)))
	require.NoError(t, err)
	require.True(t, has, "GTT TriggeredPending order must reinstate ExpiryIndex on genesis")

	has, err = k.ExpiryIndex.Has(ctx, collections.Join(int64(0), uint64(6)))
	require.NoError(t, err)
	require.False(t, has, "GTT order with Expiry=0 must NOT be indexed")

	// EndBlocker past both expiries must cancel both indexed orders.
	ctxAt := sdk.UnwrapSDKContext(ctx).WithBlockHeader(cmtprototypes.Header{Time: time.UnixMilli(500)})
	require.NoError(t, k.EndBlocker(ctxAt))

	o4, err := k.GetOrder(ctx, 4)
	require.NoError(t, err)
	require.Equal(t, perptypes.OrderStatusCancelled, o4.Status)
	o5, err := k.GetOrder(ctx, 5)
	require.NoError(t, err)
	require.Equal(t, perptypes.OrderStatusCancelled, o5.Status)
	o6, err := k.GetOrder(ctx, 6)
	require.NoError(t, err)
	require.NotEqual(t, perptypes.OrderStatusCancelled, o6.Status,
		"GTT order with Expiry=0 must survive EndBlocker indefinitely")
}
