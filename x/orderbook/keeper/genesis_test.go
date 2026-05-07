package keeper_test

import (
	"testing"

	"github.com/stretchr/testify/require"

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
	require.NoError(t, k.IterateAccountOpenOrders(ctx, open.OwnerAccountIndex, 0, func(o types.Order) bool {
		seenOpen[o.OrderIndex] = true
		return false
	}))
	require.True(t, seenOpen[open.OrderIndex])
	require.True(t, seenOpen[trigger.OrderIndex])
	require.False(t, seenOpen[cancelled.OrderIndex])

	best, ok, err := k.PeekBestOpposite(ctx, open.MarketIndex, true)
	require.NoError(t, err)
	require.True(t, ok)
	require.Equal(t, open.OrderIndex, best.OrderIndex)

	seenTrigger := false
	require.NoError(t, k.IterateTriggers(ctx, func(market uint32, triggerPrice uint32, orderIndex uint64) bool {
		seenTrigger = market == trigger.MarketIndex &&
			triggerPrice == trigger.TriggerPrice &&
			orderIndex == trigger.OrderIndex
		return seenTrigger
	}))
	require.True(t, seenTrigger)

	foundCancelled, _, err := k.HasOpenClientOrder(ctx, cancelled.MarketIndex, cancelled.OwnerAccountIndex, cancelled.ClientOrderIndex)
	require.NoError(t, err)
	require.False(t, foundCancelled)
}
