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

// TestApplyCountDelta_OverflowGuard rejects a positive delta that
// would carry the per-side entry-count past math.MaxUint32. The
// counter mirrors the proto field width (uint32) so the upper bound
// is MaxUint32, not MaxInt32.
func TestApplyCountDelta_OverflowGuard(t *testing.T) {
	_, err := orderbookkeeper.ApplyCountDelta(stdmath.MaxUint32, +1)
	require.ErrorIs(t, err, types.ErrPriceLevelOverflow)
}

// TestApplyCountDelta_UnderflowIsInvariant proves a decrement on a
// zero counter surfaces ErrInvariantViolated — the price level is
// created together with its first entry and torn down together with
// the last, so an under-subtract is a state-machine bug.
func TestApplyCountDelta_UnderflowIsInvariant(t *testing.T) {
	_, err := orderbookkeeper.ApplyCountDelta(0, -1)
	require.ErrorIs(t, err, types.ErrInvariantViolated)
}

// TestApplyCountDelta_ExactBoundary checks the legal MaxUint32 ceiling
// is reachable (the guard kicks in strictly past it).
func TestApplyCountDelta_ExactBoundary(t *testing.T) {
	got, err := orderbookkeeper.ApplyCountDelta(stdmath.MaxUint32-1, +1)
	require.NoError(t, err)
	require.EqualValues(t, uint32(stdmath.MaxUint32), got)
}

// TestCheckedQuote_ZeroBase / _ZeroPrice / _BothZero pin the smooth
// boundary at the multiplicative identity: orders / fill-bookkeeping
// frequently hand the helper (0, x) or (x, 0) and rely on a clean
// (0, nil) return rather than an error. `partialFill` in particular
// reaches this branch on full-fill cleanup.
func TestCheckedQuote_ZeroBase(t *testing.T) {
	q, err := orderbookkeeper.CheckedQuote(0, 1_000)
	require.NoError(t, err)
	require.Zero(t, q)
}

func TestCheckedQuote_ZeroPrice(t *testing.T) {
	q, err := orderbookkeeper.CheckedQuote(1_000, 0)
	require.NoError(t, err)
	require.Zero(t, q)
}

func TestCheckedQuote_BothZero(t *testing.T) {
	q, err := orderbookkeeper.CheckedQuote(0, 0)
	require.NoError(t, err)
	require.Zero(t, q)
}
