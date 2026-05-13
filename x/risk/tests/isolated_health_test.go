// isolated_health_test.go covers per-market isolated margin health
// classification (riskkeeper.GetIsolatedHealthStatus) and asserts
// that isolated health is computed independently from the cross
// aggregate health (riskkeeper.GetHealthStatus).
package tests

import (
	"testing"

	"cosmossdk.io/math"
	"github.com/stretchr/testify/require"

	perptypes "github.com/perpdex/perpdex-l1/types"
	accounttypes "github.com/perpdex/perpdex-l1/x/account/types"
	markettypes "github.com/perpdex/perpdex-l1/x/market/types"
)

// TestGetIsolatedHealthStatus_PerMarket verifies isolated positions are
// classified independently from the cross aggregate.
func TestGetIsolatedHealthStatus_PerMarket(t *testing.T) {
	ak := stubAccountKeeper{
		acc: accounttypes.Account{AccountIndex: 1, Collateral: math.NewInt(1_000_000)},
		pos: accounttypes.AccountPosition{
			AccountIndex: 1, MarketIndex: 0,
			BaseSize:                 math.NewInt(10),
			EntryQuote:               math.NewInt(11_000), // uPnL = -1_000
			LastFundingRatePrefixSum: math.ZeroInt(),
			AllocatedMargin:          math.NewInt(500),
			MarginMode:               perptypes.IsolatedMargin,
		},
	}
	mk := stubMarketKeeper{md: markettypes.MarketDetails{
		DefaultInitialMarginFraction: 1000, // 10%
		MaintenanceMarginFraction:    500,  // 5%
		CloseOutMarginFraction:       250,  // 2.5%
	}}
	mk.md.MarkPrice = 1000
	k, ctx := makeKeeper(t, &ak, mk)

	// Cross is healthy (collateral plenty), but isolated has TAV =
	// AllocatedMargin + uPnL = 500 - 1000 = -500 → BANKRUPTCY.
	cross, err := k.GetHealthStatus(ctx, 1)
	require.NoError(t, err)
	require.Equal(t, perptypes.HealthHealthy, cross,
		"cross health must be HEALTHY; isolated must not affect it")

	iso, err := k.GetIsolatedHealthStatus(ctx, 1, 0)
	require.NoError(t, err)
	require.Equal(t, perptypes.HealthBankruptcy, iso,
		"isolated TAV<0 must classify as BANKRUPTCY independently")
}
