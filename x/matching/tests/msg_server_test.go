// msg_server_test.go covers two small predicates that live in the
// msg-server but are independent of the orderbook fixture:
//
//   - QuoteExceedsLimit: the per-order quote cap that bounds the
//     base × price product via math.Int so an attacker cannot wrap an
//     int64 overflow back below the limit.
//   - IsTriggerOrder: the local replica of the trigger-order
//     classifier used to route stop/take orders into the trigger
//     index rather than the active book.
package tests

import (
	stdmath "math"
	"testing"

	"github.com/stretchr/testify/require"

	perptypes "github.com/perpdex/perpdex-l1/types"
	matchingkeeper "github.com/perpdex/perpdex-l1/x/matching/keeper"
)

// TestQuoteExceedsLimit covers the three regimes that the per-order quote
// cap must handle: no limit, under limit, and the int64-overflow fast path.
func TestQuoteExceedsLimit(t *testing.T) {
	tests := []struct {
		name  string
		base  uint64
		price uint32
		limit int64
		want  bool
	}{
		{"no limit", 1_000_000, 1_000_000, 0, false},
		{"under limit", 100, 100, 20_000, false},
		{"at limit", 100, 200, 20_000, false},
		{"above limit", 100, 201, 20_000, true},
		{"overflow short-circuit", uint64(1 << 33), uint32(stdmath.MaxUint32), 1 << 62, true},
		{"price zero", 1_000_000_000_000, 0, 1, false},
		{"near int64 max", uint64(stdmath.MaxInt32), uint32(stdmath.MaxInt32 - 1), stdmath.MaxInt64, false},
		// A legal base ~2^48 multiplied by a modest price wraps an
		// int64 product back below `limit`; the math.Int check must
		// still detect the overflow.
		{"legal base overflow", uint64(1 << 48), uint32(1 << 16), 1_000_000, true},
		// near-uint64-max base must also overflow detection-wise.
		{"max base", stdmath.MaxUint64, 1_000_000, 1_000_000_000, true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := matchingkeeper.QuoteExceedsLimit(tc.base, tc.price, tc.limit)
			require.Equal(t, tc.want, got)
		})
	}
}

// TestIsTriggerOrder verifies the matching-side predicate used to route
// stop/take orders into the trigger index instead of the active book.
func TestIsTriggerOrder(t *testing.T) {
	require.True(t, matchingkeeper.IsTriggerOrder(perptypes.StopLossOrder))
	require.True(t, matchingkeeper.IsTriggerOrder(perptypes.StopLossLimitOrder))
	require.True(t, matchingkeeper.IsTriggerOrder(perptypes.TakeProfitOrder))
	require.True(t, matchingkeeper.IsTriggerOrder(perptypes.TakeProfitLimitOrder))
	require.False(t, matchingkeeper.IsTriggerOrder(perptypes.LimitOrder))
	require.False(t, matchingkeeper.IsTriggerOrder(perptypes.MarketOrder))
}
