package keeper

import (
	"math"
	"testing"

	"github.com/stretchr/testify/require"
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
		{"overflow short-circuit", uint64(1 << 33), uint32(math.MaxUint32), 1 << 62, true},
		{"price zero", 1_000_000_000_000, 0, 1, false},
		{"near int64 max", uint64(math.MaxInt32), uint32(math.MaxInt32 - 1), math.MaxInt64, false},
		// historical bypass: legal base ~2^48 multiplied by a modest
		// price wraps an int64 product back below `limit`, but the
		// big.Int implementation must still detect the overflow.
		{"legal base overflow", uint64(1 << 48), uint32(1 << 16), 1_000_000, true},
		// near-uint64-max base must also overflow detection-wise.
		{"max base", math.MaxUint64, 1_000_000, 1_000_000_000, true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := quoteExceedsLimit(tc.base, tc.price, tc.limit)
			require.Equal(t, tc.want, got)
		})
	}
}

// TestIsTriggerOrder verifies the matching-side predicate used to route
// stop/take orders into the trigger index instead of the active book.
func TestIsTriggerOrder(t *testing.T) {
	require.True(t, isTriggerOrder(perptypesStopLoss))
	require.True(t, isTriggerOrder(perptypesStopLossLimit))
	require.True(t, isTriggerOrder(perptypesTakeProfit))
	require.True(t, isTriggerOrder(perptypesTakeProfitLimit))
	require.False(t, isTriggerOrder(perptypesLimit))
	require.False(t, isTriggerOrder(perptypesMarket))
}

// Local aliases avoid importing constants just for a small predicate test.
const (
	perptypesLimit           = uint32(0)
	perptypesMarket          = uint32(1)
	perptypesStopLoss        = uint32(2)
	perptypesStopLossLimit   = uint32(3)
	perptypesTakeProfit      = uint32(4)
	perptypesTakeProfitLimit = uint32(5)
)
