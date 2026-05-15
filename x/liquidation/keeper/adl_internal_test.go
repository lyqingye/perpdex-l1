package keeper

import (
	"testing"

	"cosmossdk.io/math"

	"github.com/stretchr/testify/require"

	risktypes "github.com/perpdex/perpdex-l1/x/risk/types"
)

// TestZeroPriceMid_Rounding pins the direction-aware midpoint: long
// victim → ceiling, short victim → floor. Even sums round the same
// way; only odd sums exercise the asymmetry.
func TestZeroPriceMid_Rounding(t *testing.T) {
	cases := []struct {
		name         string
		a, b         uint32
		victimIsLong bool
		want         uint32
	}{
		{"even sum, victim long", 100, 200, true, 150},
		{"even sum, victim short", 100, 200, false, 150},
		{"odd sum, victim long ceil", 100, 105, true, 103},
		{"odd sum, victim short floor", 100, 105, false, 102},
		{"equal endpoints, victim long", 100, 100, true, 100},
		{"equal endpoints, victim short", 100, 100, false, 100},
		{"min, victim long", 0, 1, true, 1},
		{"min, victim short", 0, 1, false, 0},
		{"max range, victim long", 4_294_967_293, 4_294_967_295, true, 4_294_967_294},
		{"max range, victim short", 4_294_967_293, 4_294_967_295, false, 4_294_967_294},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			require.Equal(t, tc.want, zeroPriceMid(tc.a, tc.b, tc.victimIsLong))
		})
	}
}

// TestComputeLeverage_Edges covers the three branches: IM=0 → 1,
// Collateral<=0 clamps to 1 (front of queue), and the nominal ratio.
func TestComputeLeverage_Edges(t *testing.T) {
	cases := []struct {
		name string
		rp   risktypes.RiskParameters
		want math.Int
	}{
		{
			name: "im=0 returns neutral 1",
			rp: risktypes.RiskParameters{
				Collateral:               math.NewInt(1_000_000),
				InitialMarginRequirement: math.ZeroInt(),
			},
			want: math.OneInt(),
		},
		{
			name: "collateral=0 clamped to 1",
			rp: risktypes.RiskParameters{
				Collateral:               math.ZeroInt(),
				InitialMarginRequirement: math.NewInt(7),
			},
			want: math.NewInt(7 * 10_000),
		},
		{
			name: "collateral negative clamped to 1",
			rp: risktypes.RiskParameters{
				Collateral:               math.NewInt(-5),
				InitialMarginRequirement: math.NewInt(3),
			},
			want: math.NewInt(3 * 10_000),
		},
		{
			name: "nominal ratio",
			rp: risktypes.RiskParameters{
				Collateral:               math.NewInt(1_000),
				InitialMarginRequirement: math.NewInt(100),
			},
			want: math.NewInt(100 * 10_000 / 1_000),
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			require.True(t, tc.want.Equal(computeLeverage(tc.rp)),
				"want=%s got=%s", tc.want.String(), computeLeverage(tc.rp).String())
		})
	}
}

// TestComputeLeverage_PanicsOnNilCollateral pins the invariant guard:
// nil Collateral panics rather than silently degrading rank to 1.
func TestComputeLeverage_PanicsOnNilCollateral(t *testing.T) {
	rp := risktypes.RiskParameters{
		InitialMarginRequirement: math.NewInt(1),
	}
	require.True(t, rp.Collateral.IsNil())
	require.Panics(t, func() { _ = computeLeverage(rp) })
}
