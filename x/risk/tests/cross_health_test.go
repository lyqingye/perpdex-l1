// cross_health_test.go covers the cross-margin risk aggregate built
// by riskkeeper.ComputeCrossRisk: mark-price gating (zero / stale
// guards) and the cross-vs-isolated isolation invariant which
// guarantees isolated PnL / margin never leaks into the cross
// aggregate.
package tests

import (
	"testing"

	"cosmossdk.io/math"
	"github.com/stretchr/testify/require"

	perptypes "github.com/perpdex/perpdex-l1/types"
	accounttypes "github.com/perpdex/perpdex-l1/x/account/types"
	markettypes "github.com/perpdex/perpdex-l1/x/market/types"
)

// TestComputeCrossRisk_StalePriceFailsClosed pins the invariant that
// a non-zero position with a stale MarketDetails.MarkPrice surfaces
// an explicit error instead of being silently skipped. The
// authoritative mark is written every block by the funding
// BeginBlocker; if that pipeline has not refreshed within the
// governance-configured window we MUST fail-closed instead of pricing
// PnL / IM / MM with a stale value.
func TestComputeCrossRisk_StalePriceFailsClosed(t *testing.T) {
	ak := stubAccountKeeper{
		acc: accounttypes.Account{AccountIndex: 1, Collateral: math.NewInt(1000)},
		pos: accounttypes.AccountPosition{
			AccountIndex: 1, MarketIndex: 0,
			BaseSize: math.NewInt(5), EntryQuote: math.NewInt(500),
			LastFundingRatePrefixSum: math.ZeroInt(), AllocatedMargin: math.ZeroInt(),
		},
	}
	// Non-zero mark with a LastMarkPriceRefreshTimestamp that is "now - 1h" so
	// the staleness gate trips against the 1-minute stub.
	mk := stubMarketKeeper{
		md: markettypes.MarketDetails{
			MarkPrice:                     100,
			LastMarkPriceRefreshTimestamp: 0, // never refreshed by funding pipeline
		},
		maxStaleness: 60_000, // 1m
	}
	k, ctx := makeKeeper(t, &ak, mk)

	_, err := k.ComputeCrossRisk(ctx, 1)
	require.ErrorIs(t, err, markettypes.ErrStaleMarkPrice)
}

// TestComputeCrossRisk_ZeroMarkPriceRejected enforces the "non-zero mark"
// invariant on non-zero positions.
func TestComputeCrossRisk_ZeroMarkPriceRejected(t *testing.T) {
	ak := stubAccountKeeper{
		acc: accounttypes.Account{AccountIndex: 1, Collateral: math.NewInt(1000)},
		pos: accounttypes.AccountPosition{
			AccountIndex: 1, MarketIndex: 0,
			BaseSize: math.NewInt(5), EntryQuote: math.NewInt(500),
			LastFundingRatePrefixSum: math.ZeroInt(), AllocatedMargin: math.ZeroInt(),
		},
	}
	mk := stubMarketKeeper{md: markettypes.MarketDetails{}}
	k, ctx := makeKeeper(t, &ak, mk)

	_, err := k.ComputeCrossRisk(ctx, 1)
	require.ErrorIs(t, err, markettypes.ErrZeroMarkPrice)
}

// TestComputeCrossRisk_IsolatedDoesNotPolluteCross pins the invariant
// that isolated positions are excluded from the cross aggregate: an
// isolated position's AllocatedMargin / uPnL MUST NOT contribute to
// cross TAV, IM, MM or CM, otherwise isolated profit could silently
// inflate cross health.
func TestComputeCrossRisk_IsolatedDoesNotPolluteCross(t *testing.T) {
	ak := stubAccountKeeper{
		acc: accounttypes.Account{AccountIndex: 1, Collateral: math.NewInt(100)},
		pos: accounttypes.AccountPosition{
			AccountIndex: 1, MarketIndex: 0,
			BaseSize:                 math.NewInt(10),
			EntryQuote:               math.NewInt(9_000), // uPnL = 1_000 (large profit)
			LastFundingRatePrefixSum: math.ZeroInt(),
			AllocatedMargin:          math.NewInt(50),
			MarginMode:               perptypes.IsolatedMargin,
		},
	}
	mk := stubMarketKeeper{md: markettypes.MarketDetails{
		DefaultInitialMarginFraction: 100,
		MaintenanceMarginFraction:    100,
		CloseOutMarginFraction:       50,
	}}
	mk.md.MarkPrice = 1000
	k, ctx := makeKeeper(t, &ak, mk)

	rp, err := k.ComputeCrossRisk(ctx, 1)
	require.NoError(t, err)
	require.Equal(t, "100", rp.TotalAccountValue.String(),
		"cross TAV must equal cross collateral; isolated profit must not leak in")
	require.True(t, rp.MaintenanceMarginRequirement.IsZero(),
		"isolated MMR must not aggregate into cross MMR")
}
