// match_order_test.go covers the user-driven matching kernel
// (`keeper.Keeper.MatchOrder`) for ordinary limit / market takers.
// Recovery from maker/taker rejections lives in
// `match_recovery_test.go`; reduce-only invariants live in
// `reduce_only_test.go`; liquidation IOC matching lives in
// `match_liquidation_test.go`.
package tests

import (
	"testing"

	"github.com/stretchr/testify/require"

	perptypes "github.com/perpdex/perpdex-l1/types"
	orderbooktypes "github.com/perpdex/perpdex-l1/x/orderbook/types"
)

// TestMatchOrder_MarketOrderBidAtZeroPrice pins the invariant that a
// buy MarketOrder with Price=0 (the canonical no-limit-price form,
// also produced by activated STOP/TAKE triggers) is accepted by the
// limit-price gate.
func TestMatchOrder_MarketOrderBidAtZeroPrice(t *testing.T) {
	e := newMatchEnv(t)

	maker := orderbooktypes.Order{
		OrderIndex:          1,
		OwnerAccountIndex:   10,
		MarketIndex:         1,
		IsAsk:               true,
		OrderType:           perptypes.LimitOrder,
		TimeInForce:         perptypes.GTT,
		Price:               1000,
		Nonce:               1,
		InitialBaseAmount:   5,
		RemainingBaseAmount: 5,
		Status:              perptypes.OrderStatusOpen,
		Expiry:              0,
	}
	e.rest(t, maker, true)

	taker := &orderbooktypes.Order{
		OrderIndex:          2,
		OwnerAccountIndex:   20,
		MarketIndex:         1,
		IsAsk:               false,
		OrderType:           perptypes.MarketOrder,
		TimeInForce:         perptypes.IOC,
		Price:               0, // no-limit-price
		Nonce:               2,
		InitialBaseAmount:   5,
		RemainingBaseAmount: 5,
		Status:              perptypes.OrderStatusOpen,
	}

	filled, status, err := e.k.MatchOrder(e.ctx, taker, 16)
	require.NoError(t, err)
	require.EqualValues(t, 5, filled)
	require.Equal(t, perptypes.OrderStatusFilled, status)
	require.Len(t, e.tk.fills, 1)
	require.EqualValues(t, 5, e.tk.fills[0].BaseAmount)
}
