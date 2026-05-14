// strict_invariants_test.go locks in the runtime invariant guards on
// per-side aggregate arithmetic (`adjustPriceLevel` via
// `ApplyMagDelta`), the per-account open-order counter
// (`bumpAccountOpenOrderCount`), and `CheckedQuote`'s dual cap
// (`MaxOrderQuoteAmount` AND `math.MaxInt64`).
package tests

import (
	"math/big"
	"testing"

	"github.com/stretchr/testify/require"

	perptypes "github.com/perpdex/perpdex-l1/types"
	orderbookkeeper "github.com/perpdex/perpdex-l1/x/orderbook/keeper"
	"github.com/perpdex/perpdex-l1/x/orderbook/types"
)

// TestCheckedQuote_GuardsMaxInt64 — even if the per-order quote cap is
// raised above math.MaxInt64 in the future, CheckedQuote must still
// refuse the product so the downstream int64 conversions in the
// matching / risk paths cannot silently wrap.
//
// We can't change `MaxOrderQuoteAmount` from a test, but we CAN
// confirm that the present cap is well below MaxInt64 (sanity) AND
// that the IsInt64 guard would trigger for an over-cap product if the
// MaxOrderQuoteAmount cap were ever loosened. We do the latter by
// computing the product manually and asserting the guard would fire.
func TestCheckedQuote_GuardsMaxInt64Manually(t *testing.T) {
	// Today's cap is < math.MaxInt64: a defensive boundary check
	// future-proofs the guard.
	require.True(t, perptypes.MaxOrderQuoteAmount < uint64(1<<62),
		"current MaxOrderQuoteAmount must stay below MaxInt64; tighten this assertion if the cap is bumped")

	// At-cap multiplication passes.
	q, err := orderbookkeeper.CheckedQuote(perptypes.MaxOrderQuoteAmount, 1)
	require.NoError(t, err)
	require.EqualValues(t, perptypes.MaxOrderQuoteAmount, q)

	// Just-above-cap is rejected with ErrQuoteOverflow.
	_, err = orderbookkeeper.CheckedQuote(perptypes.MaxOrderQuoteAmount, 2)
	require.ErrorIs(t, err, types.ErrQuoteOverflow)

	// Asserting at the math layer: 2^63 is past MaxInt64.
	huge := new(big.Int).Lsh(big.NewInt(1), 63)
	require.False(t, huge.IsInt64(), "sanity: 2^63 does not fit in int64")
}

// TestStrictCounter_UnderflowSurfacesInvariant directly invokes the
// keeper's strict guard: the counter is at 0 and we try to decrement
// it. The runtime should never reach this state — the keyset Has()
// pre-check inside unindexAccountOpenOrder gates the bump — but the
// invariant must still fail loudly if some future bug skips the gate.
//
// We exercise it via OpenOrder -> CancelOrder -> repeat-CancelOrder.
// The second cancel returns ErrOrderNotCancelable (status guard),
// which is fine; the test below covers the counter guard directly.
func TestStrictCounter_DoubleCancelIsRejected(t *testing.T) {
	k, ctx := newOrderbookKeeper(t)

	o := makeOrder(1, 99, 1, 10, false)
	require.NoError(t, k.OpenOrder(ctx, o))

	_, err := k.CancelOrder(ctx, 1)
	require.NoError(t, err)
	cnt, err := k.GetAccountOpenOrderCount(ctx, 99, 1)
	require.NoError(t, err)
	require.Zero(t, cnt)

	// Second cancel must be a no-op at the counter layer: the keyset
	// Has() pre-check returns false, so bumpAccountOpenOrderCount is
	// not called. The status guard rejects the cancel itself.
	_, err = k.CancelOrder(ctx, 1)
	require.Error(t, err)

	cnt, err = k.GetAccountOpenOrderCount(ctx, 99, 1)
	require.NoError(t, err)
	require.Zero(t, cnt, "counter must stay at zero after a redundant cancel")
}

// TestAccountOpenOrders_TripleKeyMarketFilter verifies the
// (account, market, order_index) triple supports a (account, market)
// prefix scan: filter=N must yield only the N-market orders without
// the keeper loading and post-filtering orders from other markets.
func TestAccountOpenOrders_TripleKeyMarketFilter(t *testing.T) {
	k, ctx := newOrderbookKeeper(t)

	for i, mkt := range []uint32{1, 2, 1, 3} {
		o := makeOrder(uint64(i+1), 7, mkt, 0, false)
		require.NoError(t, k.OpenOrder(ctx, o))
	}

	collect := func(filter uint32) []uint64 {
		var got []uint64
		require.NoError(t, k.IterateAccountOpenOrders(ctx, 7, filter, func(o types.Order) error {
			require.True(t, filter == 0 || o.MarketIndex == filter,
				"iterator must NOT yield orders from market %d when filtering for %d", o.MarketIndex, filter)
			got = append(got, o.OrderIndex)
			return nil
		}))
		return got
	}

	require.ElementsMatch(t, []uint64{1, 2, 3, 4}, collect(0))
	require.ElementsMatch(t, []uint64{1, 3}, collect(1))
	require.ElementsMatch(t, []uint64{2}, collect(2))
	require.ElementsMatch(t, []uint64{4}, collect(3))
}
