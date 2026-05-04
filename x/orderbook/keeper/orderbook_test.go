package keeper

import (
	"testing"

	"github.com/stretchr/testify/require"

	perptypes "github.com/perpdex/perpdex-l1/types"
	"github.com/perpdex/perpdex-l1/x/orderbook/types"
)

// TestCheckedQuote_WithinCap accepts products at or below the configured
// per-order cap.
func TestCheckedQuote_WithinCap(t *testing.T) {
	q, err := checkedQuote(1_000, 2_000)
	require.NoError(t, err)
	require.EqualValues(t, 2_000_000, q)
}

// TestCheckedQuote_AtCap accepts the exact cap boundary.
func TestCheckedQuote_AtCap(t *testing.T) {
	q, err := checkedQuote(perptypes.MaxOrderQuoteAmount, 1)
	require.NoError(t, err)
	require.EqualValues(t, perptypes.MaxOrderQuoteAmount, q)
}

// TestCheckedQuote_Overflow rejects a product that exceeds the cap.
func TestCheckedQuote_Overflow(t *testing.T) {
	_, err := checkedQuote(perptypes.MaxOrderQuoteAmount, 2)
	require.ErrorIs(t, err, types.ErrQuoteOverflow)
}

// TestApplyQuoteDelta_OverflowGuard refuses to wrap past math.MaxUint64.
func TestApplyQuoteDelta_OverflowGuard(t *testing.T) {
	_, err := applyQuoteDelta(maxUint64-10, 11)
	require.ErrorIs(t, err, types.ErrPriceLevelOverflow)
}

// TestApplyQuoteDelta_NegativeCapped clamps a negative delta to zero rather
// than wrapping to a huge uint64.
func TestApplyQuoteDelta_NegativeCapped(t *testing.T) {
	got, err := applyQuoteDelta(50, -100)
	require.NoError(t, err)
	require.Zero(t, got)
}
