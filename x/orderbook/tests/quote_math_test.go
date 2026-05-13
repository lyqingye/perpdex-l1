// quote_math_test.go covers the two pure helpers that guard the
// orderbook price-level arithmetic from silent uint64 overflow:
// `CheckedQuote(base, price)` enforces the per-order
// `MaxOrderQuoteAmount` cap on multiplication, and
// `ApplyQuoteDelta(cur, delta)` adds a signed delta to a price-level
// aggregate while rejecting positive sums that would wrap past
// `MaxUint64` and clamping over-subtraction to zero.
package tests

import (
	"testing"

	"github.com/stretchr/testify/require"

	perptypes "github.com/perpdex/perpdex-l1/types"
	orderbookkeeper "github.com/perpdex/perpdex-l1/x/orderbook/keeper"
	"github.com/perpdex/perpdex-l1/x/orderbook/types"
)

// TestCheckedQuote_WithinCap accepts products at or below the configured
// per-order cap.
func TestCheckedQuote_WithinCap(t *testing.T) {
	q, err := orderbookkeeper.CheckedQuote(1_000, 2_000)
	require.NoError(t, err)
	require.EqualValues(t, 2_000_000, q)
}

// TestCheckedQuote_AtCap accepts the exact cap boundary.
func TestCheckedQuote_AtCap(t *testing.T) {
	q, err := orderbookkeeper.CheckedQuote(perptypes.MaxOrderQuoteAmount, 1)
	require.NoError(t, err)
	require.EqualValues(t, perptypes.MaxOrderQuoteAmount, q)
}

// TestCheckedQuote_Overflow rejects a product that exceeds the cap.
func TestCheckedQuote_Overflow(t *testing.T) {
	_, err := orderbookkeeper.CheckedQuote(perptypes.MaxOrderQuoteAmount, 2)
	require.ErrorIs(t, err, types.ErrQuoteOverflow)
}

// TestApplyQuoteDelta_OverflowGuard refuses to wrap past math.MaxUint64.
func TestApplyQuoteDelta_OverflowGuard(t *testing.T) {
	_, err := orderbookkeeper.ApplyQuoteDelta(orderbookkeeper.MaxUint64-10, 11)
	require.ErrorIs(t, err, types.ErrPriceLevelOverflow)
}

// TestApplyQuoteDelta_NegativeCapped clamps a negative delta to zero rather
// than wrapping to a huge uint64.
func TestApplyQuoteDelta_NegativeCapped(t *testing.T) {
	got, err := orderbookkeeper.ApplyQuoteDelta(50, -100)
	require.NoError(t, err)
	require.Zero(t, got)
}
