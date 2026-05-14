// iterator_contract_test.go pins the callback-return-error contract on
// IterateAccountOpenOrders and IterateTriggers:
//
//   - returning `nil` continues iteration
//   - returning `types.ErrStopIteration` terminates cleanly (iterator
//     returns nil to the caller)
//   - returning ANY OTHER error aborts iteration and propagates verbatim
//
// The contract lets call sites surface real errors from inside the
// callback (e.g. GetMarketDetails failure during trigger sweep) instead
// of swallowing them into a closure-captured variable, while still
// providing a cheap "I have what I need, stop now" early-exit path.
package tests

import (
	"errors"
	"testing"

	"github.com/stretchr/testify/require"

	perptypes "github.com/perpdex/perpdex-l1/types"
	"github.com/perpdex/perpdex-l1/x/orderbook/types"
)

// makeTrigger constructs a trigger-pending order keyed off `idx`.
// `triggerPrice` is the per-order trigger price the keeper indexes
// the order under; `clientID` is preserved verbatim so a test can
// confirm the full Order surfaces from IterateTriggers (4-B).
func makeTrigger(idx uint64, account uint64, market uint32, triggerPrice uint32, clientID uint64, isAsk bool) types.Order {
	o := makeOrder(idx, account, market, clientID, isAsk)
	o.Status = perptypes.OrderStatusTriggeredPending
	o.OrderType = perptypes.StopLossLimitOrder
	o.TriggerPrice = triggerPrice
	return o
}

func TestIterateAccountOpenOrders_StopIterationTerminatesCleanly(t *testing.T) {
	k, ctx := newOrderbookKeeper(t)
	for i := 1; i <= 5; i++ {
		require.NoError(t, k.OpenOrder(ctx, makeOrder(uint64(i), 1, 1, uint64(i*100), false)))
	}

	var seen []uint64
	err := k.IterateAccountOpenOrders(ctx, 1, 0, func(o types.Order) error {
		seen = append(seen, o.OrderIndex)
		if len(seen) == 2 {
			return types.ErrStopIteration
		}
		return nil
	})
	require.NoError(t, err, "ErrStopIteration must NOT surface to the caller")
	require.Len(t, seen, 2, "iteration must terminate at the first ErrStopIteration return")
}

func TestIterateAccountOpenOrders_PropagatesArbitraryError(t *testing.T) {
	k, ctx := newOrderbookKeeper(t)
	for i := 1; i <= 3; i++ {
		require.NoError(t, k.OpenOrder(ctx, makeOrder(uint64(i), 1, 1, uint64(i*100), false)))
	}

	sentinel := errors.New("callback exploded")
	var seen int
	err := k.IterateAccountOpenOrders(ctx, 1, 0, func(_ types.Order) error {
		seen++
		if seen == 2 {
			return sentinel
		}
		return nil
	})
	require.ErrorIs(t, err, sentinel, "arbitrary callback errors must surface verbatim")
	require.Equal(t, 2, seen, "iteration must stop at the failing callback")
}

func TestIterateTriggers_StopIterationTerminatesCleanly(t *testing.T) {
	k, ctx := newOrderbookKeeper(t)
	for i := 1; i <= 4; i++ {
		o := makeTrigger(uint64(i), 1, 1, uint32(100+i), uint64(i*10), false)
		require.NoError(t, k.OpenTriggerOrder(ctx, o))
	}

	var seen []uint64
	err := k.IterateTriggers(ctx, func(o types.Order) error {
		seen = append(seen, o.OrderIndex)
		if len(seen) == 2 {
			return types.ErrStopIteration
		}
		return nil
	})
	require.NoError(t, err, "ErrStopIteration must NOT surface")
	require.Len(t, seen, 2)
}

func TestIterateTriggers_PropagatesArbitraryError(t *testing.T) {
	k, ctx := newOrderbookKeeper(t)
	require.NoError(t, k.OpenTriggerOrder(ctx, makeTrigger(1, 1, 1, 101, 10, false)))
	require.NoError(t, k.OpenTriggerOrder(ctx, makeTrigger(2, 1, 1, 102, 20, false)))

	sentinel := errors.New("trigger callback exploded")
	err := k.IterateTriggers(ctx, func(_ types.Order) error {
		return sentinel
	})
	require.ErrorIs(t, err, sentinel)
}

// TestIterateTriggers_CallbackReceivesFullOrder pins the 4-B contract:
// the trigger callback observes the full Order (market / trigger price
// / order index / type), not just the triple index. This means the
// matching EndBlocker can read activation semantics off `o` directly
// without a second GetOrder round-trip.
func TestIterateTriggers_CallbackReceivesFullOrder(t *testing.T) {
	k, ctx := newOrderbookKeeper(t)
	want := makeTrigger(7, 42, 3, 1234, 99, true)
	require.NoError(t, k.OpenTriggerOrder(ctx, want))

	var got types.Order
	require.NoError(t, k.IterateTriggers(ctx, func(o types.Order) error {
		got = o
		return types.ErrStopIteration
	}))
	require.Equal(t, want.OrderIndex, got.OrderIndex)
	require.Equal(t, want.MarketIndex, got.MarketIndex)
	require.Equal(t, want.TriggerPrice, got.TriggerPrice)
	require.Equal(t, want.OwnerAccountIndex, got.OwnerAccountIndex)
	require.Equal(t, want.IsAsk, got.IsAsk)
	require.Equal(t, want.OrderType, got.OrderType)
}
