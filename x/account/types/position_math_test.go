package types_test

import (
	"testing"

	"cosmossdk.io/math"
	"github.com/stretchr/testify/require"

	"github.com/perpdex/perpdex-l1/x/account/types"
)

// TestAccountPosition_MarketValue covers the lighter `market_value`
// formula AllocatedMargin + UnrealizedPnL(mark) used by
// calculateIsolatedMarginDelta case 4.
func TestAccountPosition_MarketValue(t *testing.T) {
	t.Run("empty_position_returns_allocated_only", func(t *testing.T) {
		p := types.AccountPosition{
			BaseSize:        math.ZeroInt(),
			EntryQuote:      math.ZeroInt(),
			AllocatedMargin: math.NewInt(500),
		}
		require.Equal(t, math.NewInt(500), p.MarketValue(100))
	})

	t.Run("zero_mark_returns_allocated_only", func(t *testing.T) {
		p := types.AccountPosition{
			BaseSize:        math.NewInt(10),
			EntryQuote:      math.NewInt(900),
			AllocatedMargin: math.NewInt(500),
		}
		require.Equal(t, math.NewInt(500), p.MarketValue(0))
	})

	t.Run("long_in_profit", func(t *testing.T) {
		// uPnL = 10 * 100 - 900 = 100, MV = 500 + 100 = 600.
		p := types.AccountPosition{
			BaseSize:        math.NewInt(10),
			EntryQuote:      math.NewInt(900),
			AllocatedMargin: math.NewInt(500),
		}
		require.Equal(t, math.NewInt(600), p.MarketValue(100))
	})

	t.Run("nil_allocated_treated_as_zero", func(t *testing.T) {
		// Mirror NormalizeIntFields behaviour without invoking it: nil
		// AllocatedMargin should not panic — MarketValue must coerce
		// to zero.
		p := types.AccountPosition{
			BaseSize:   math.NewInt(10),
			EntryQuote: math.NewInt(900),
		}
		// uPnL = 100, allocated coerced to 0, MV = 100.
		require.Equal(t, math.NewInt(100), p.MarketValue(100))
	})
}

func TestAccountPosition_DirectionHelpers(t *testing.T) {
	tests := []struct {
		name string
		size math.Int
		want bool
	}{
		{
			name: "short_size_is_not_long_and_opens_ask",
			size: math.NewInt(-1),
			want: false,
		},
		{
			name: "zero_size_is_long_and_opens_bid",
			size: math.ZeroInt(),
			want: true,
		},
		{
			name: "positive_size_is_long_and_opens_bid",
			size: math.NewInt(1),
			want: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			p := types.AccountPosition{BaseSize: tt.size}
			require.Equal(t, tt.want, p.IsLong())
			require.Equal(t, tt.want, p.OpeningIsBid())
		})
	}
}
