// quote_math_test.go covers the two pure helpers that guard the
// orderbook price-level arithmetic from silent uint64 overflow:
// `CheckedQuote(base, price)` enforces the per-order
// `MaxOrderQuoteAmount` cap on multiplication, and
// `ApplyMagDelta(cur, mag, sign)` applies a signed delta to a
// price-level aggregate while rejecting positive sums that would wrap
// past `math.MaxUint64` AND rejecting negative sums that would
// underflow — under-subtraction is an invariant violation because each
// removal is paired 1:1 with a prior insertion of the same magnitude.
package tests

import (
	stdmath "math"
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

// TestApplyMagDelta_OverflowGuard refuses to wrap past math.MaxUint64.
func TestApplyMagDelta_OverflowGuard(t *testing.T) {
	_, err := orderbookkeeper.ApplyMagDelta(stdmath.MaxUint64-10, 11, +1)
	require.ErrorIs(t, err, types.ErrPriceLevelOverflow)
}

// TestApplyMagDelta_NegativeUnderflowIsInvariant verifies that
// removing more than was inserted surfaces ErrInvariantViolated
// instead of silently clamping to zero.
func TestApplyMagDelta_NegativeUnderflowIsInvariant(t *testing.T) {
	_, err := orderbookkeeper.ApplyMagDelta(50, 100, -1)
	require.ErrorIs(t, err, types.ErrInvariantViolated)
}

// TestApplyMagDelta_ExactSubtractOk admits removing exactly the
// contribution that was added.
func TestApplyMagDelta_ExactSubtractOk(t *testing.T) {
	got, err := orderbookkeeper.ApplyMagDelta(100, 100, -1)
	require.NoError(t, err)
	require.Zero(t, got)
}

// TestApplyMagDelta_ZeroSignIsNoop confirms a sign of 0 leaves the
// aggregate untouched regardless of magnitude.
func TestApplyMagDelta_ZeroSignIsNoop(t *testing.T) {
	got, err := orderbookkeeper.ApplyMagDelta(42, 7, 0)
	require.NoError(t, err)
	require.EqualValues(t, 42, got)
}
