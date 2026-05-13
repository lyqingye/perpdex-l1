// types_helpers_test.go covers the pure value-object helpers attached
// to MarketDetails — currently the canonical InitialMargin formula and
// its three short-circuit paths (zero size / zero mark / zero
// fraction). These tests pin the formula independently of any
// keeper state so the math contract stays stable across refactors.
package tests

import (
	"testing"

	"cosmossdk.io/math"
	"github.com/stretchr/testify/require"

	perptypes "github.com/perpdex/perpdex-l1/types"
	"github.com/perpdex/perpdex-l1/x/market/types"
)

// TestMarketDetails_InitialMargin covers the three short-circuit paths
// (zero size / zero mark / zero fraction) plus the canonical formula
// IM = |posAbs| * mark * IMF / MarginTick.
func TestMarketDetails_InitialMargin(t *testing.T) {
	t.Run("zero_size_returns_zero", func(t *testing.T) {
		md := types.MarketDetails{DefaultInitialMarginFraction: 1_000}
		require.True(t, md.InitialMargin(math.ZeroInt(), 100).IsZero())
	})

	t.Run("zero_mark_returns_zero", func(t *testing.T) {
		md := types.MarketDetails{DefaultInitialMarginFraction: 1_000}
		require.True(t, md.InitialMargin(math.NewInt(10), 0).IsZero())
	})

	t.Run("zero_fraction_returns_zero", func(t *testing.T) {
		md := types.MarketDetails{DefaultInitialMarginFraction: 0}
		require.True(t, md.InitialMargin(math.NewInt(10), 100).IsZero())
	})

	t.Run("canonical_formula", func(t *testing.T) {
		// IM = 10 * 100 * 1000 / 10000 = 100
		require.EqualValues(t, perptypes.MarginTick, 10_000, "MarginTick assumption")
		md := types.MarketDetails{DefaultInitialMarginFraction: 1_000}
		got := md.InitialMargin(math.NewInt(10), 100)
		require.Equal(t, math.NewInt(100), got)
	})

	t.Run("nil_posAbs_treated_as_zero", func(t *testing.T) {
		md := types.MarketDetails{DefaultInitialMarginFraction: 1_000}
		require.True(t, md.InitialMargin(math.Int{}, 100).IsZero())
	})
}
