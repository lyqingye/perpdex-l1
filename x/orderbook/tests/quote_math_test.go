// quote_math_test.go covers the two pure helpers that guard the
// orderbook price-level arithmetic from silent uint64 overflow:
// `CheckedQuote(base, price)` enforces the per-order
// `MaxOrderQuoteAmount` cap on multiplication, and
// `ApplyQuoteDelta(cur, delta)` adds a signed delta to a price-level
// aggregate while rejecting positive sums that would wrap past
// `math.MaxUint64` AND rejecting negative sums that would underflow
// (the latter case is treated as an invariant violation rather than a
// silent clamp because the runtime always removes contributions of
// the exact size they were inserted with).
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

// TestApplyQuoteDelta_OverflowGuard refuses to wrap past math.MaxUint64.
func TestApplyQuoteDelta_OverflowGuard(t *testing.T) {
	_, err := orderbookkeeper.ApplyQuoteDelta(stdmath.MaxUint64-10, 11)
	require.ErrorIs(t, err, types.ErrPriceLevelOverflow)
}

// TestApplyQuoteDelta_NegativeUnderflowIsInvariant verifies that
// removing more than was inserted surfaces ErrInvariantViolated
// instead of silently clamping to zero. The orderbook only ever
// adjusts a price-level aggregate by amounts it previously added, so
// an under-subtract represents a state-machine drift the runtime must
// fail loudly on.
func TestApplyQuoteDelta_NegativeUnderflowIsInvariant(t *testing.T) {
	_, err := orderbookkeeper.ApplyQuoteDelta(50, -100)
	require.ErrorIs(t, err, types.ErrInvariantViolated)
}

// TestApplyQuoteDelta_ExactSubtractOk confirms the strict path still
// admits removing exactly the contribution that was added.
func TestApplyQuoteDelta_ExactSubtractOk(t *testing.T) {
	got, err := orderbookkeeper.ApplyQuoteDelta(100, -100)
	require.NoError(t, err)
	require.Zero(t, got)
}
