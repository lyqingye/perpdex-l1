// risk_change_test.go covers the post-trade risk-change validator
// (riskkeeper.IsValidRiskChangeFrom) and the matching pre-state
// snapshot (riskkeeper.SnapshotRisk). It pins the fail-closed
// behaviour on a missing pre-state, the no-size-up rule for accounts
// already in PRE_LIQUIDATION, and the reduce-only escape that keeps
// such accounts able to de-risk.
package tests

import (
	"testing"

	"cosmossdk.io/math"
	"github.com/stretchr/testify/require"

	perptypes "github.com/perpdex/perpdex-l1/types"
	accounttypes "github.com/perpdex/perpdex-l1/x/account/types"
	markettypes "github.com/perpdex/perpdex-l1/x/market/types"
	risktypes "github.com/perpdex/perpdex-l1/x/risk/types"
)

// TestIsValidRiskChangeFrom_NoPreStateFailClosed verifies that an
// unhealthy post state without a pre-state snapshot fails closed.
func TestIsValidRiskChangeFrom_NoPreStateFailClosed(t *testing.T) {
	// Position bought at 100_000 but mark is 10_000, so the account is
	// deeply under water → BANKRUPTCY in the post-state.
	ak := stubAccountKeeper{
		acc: accounttypes.Account{AccountIndex: 1, Collateral: math.NewInt(10)},
		pos: accounttypes.AccountPosition{
			AccountIndex: 1, MarketIndex: 0,
			BaseSize: math.NewInt(1_000_000), EntryQuote: math.NewInt(100_000_000_000),
			LastFundingRatePrefixSum: math.ZeroInt(), AllocatedMargin: math.ZeroInt(),
		},
	}
	mk := stubMarketKeeper{md: markettypes.MarketDetails{
		DefaultInitialMarginFraction: 1000, MaintenanceMarginFraction: 500,
		CloseOutMarginFraction: 250,
	}}
	mk.md.MarkPrice = 10_000
	k, ctx := makeKeeper(t, &ak, mk)

	ok2, err := k.IsValidRiskChangeFrom(ctx, 1, risktypes.PreRiskSnapshot{})
	require.NoError(t, err)
	require.False(t, ok2)
}

// TestIsValidRiskChangeFrom_PreLiquidationRejectsMMRGrowth verifies
// the PRE rule. With pre.MMR snapshotted, a post-state with strictly
// larger MMR (i.e. account opened a new position) must be rejected
// even if post is still PRE_LIQUIDATION.
func TestIsValidRiskChangeFrom_PreLiquidationRejectsMMRGrowth(t *testing.T) {
	ak := stubAccountKeeper{
		acc: accounttypes.Account{AccountIndex: 1, Collateral: math.NewInt(1_000)},
		pos: accounttypes.AccountPosition{
			AccountIndex: 1, MarketIndex: 0,
			BaseSize:                 math.NewInt(20),
			EntryQuote:               math.NewInt(-19_900),
			LastFundingRatePrefixSum: math.ZeroInt(),
			AllocatedMargin:          math.ZeroInt(),
			MarginMode:               perptypes.CrossMargin,
		},
	}
	mk := stubMarketKeeper{md: markettypes.MarketDetails{
		DefaultInitialMarginFraction: 1000, // 10%
		MaintenanceMarginFraction:    500,  // 5%
		CloseOutMarginFraction:       250,  // 2.5%
	}}
	// notional = 20*1000 = 20_000; IM = 2_000; MM = 1_000; CM = 500.
	// uPnL = 20*1000 - (-19_900) = 39_900. Way too healthy.
	// Adjust entryQuote so TAV is between MM and IM (PRE):
	// We want collateral + uPnL = 1500 (between MM=1000 and IM=2000).
	// uPnL = 1500 - 1000 = 500 ⇒ entryQuote = pos*mark - uPnL =
	//     20_000 - 500 = 19_500 ⇒ stored as positive (we paid 19_500).
	// Wait sign: long means EntryQuote should be NEGATIVE in our
	// convention (you "spent" quote). Adjust: EntryQuote = -19_500 for
	// a long means uPnL = pos*mark - EntryQuote = 20_000 - (-19_500) =
	// 39_500. That doesn't help.
	// Use entry sign as +: EntryQuote = 19_500 ⇒ uPnL = 20_000 -
	// 19_500 = 500. TAV = 1_000 + 500 = 1_500 (PRE).
	ak.pos.EntryQuote = math.NewInt(19_500)
	mk.md.MarkPrice = 1000
	k, ctx := makeKeeper(t, &ak, mk)

	// Snapshot pre-state: PRE class.
	pre, err := k.SnapshotRisk(ctx, 1)
	require.NoError(t, err)
	preStatus, err := k.GetHealthStatus(ctx, 1)
	require.NoError(t, err)
	require.Equal(t, perptypes.HealthPreLiquidation, preStatus)

	// Mutate post-state by growing the position. Same mark, so MMR
	// scales linearly with |position|. Increase position from 20 to
	// 30 → MMR grows from 1_000 to 1_500 → must be rejected.
	ak.pos.BaseSize = math.NewInt(30)
	// Keep TAV roughly in PRE range to isolate the MMR-growth signal.
	// TAV must still be < IM; IM grows from 2_000 to 3_000. Choose
	// uPnL such that TAV stays between MM(=1_500) and IM(=3_000).
	// collateral=1000, uPnL=1500 → TAV=2500 (PRE). uPnL=pos*mark -
	// entry ⇒ entry = 30_000 - 1500 = 28_500.
	ak.pos.EntryQuote = math.NewInt(28_500)
	ok2, err := k.IsValidRiskChangeFrom(ctx, 1, pre)
	require.NoError(t, err)
	require.False(t, ok2,
		"PRE → PRE with larger MMR must be rejected (no-size-up rule)")
}

// TestIsValidRiskChangeFrom_PreLiquidationAllowsReduceOnly verifies
// the inverse: if MMR shrinks while still in PRE, the change is
// accepted.
func TestIsValidRiskChangeFrom_PreLiquidationAllowsReduceOnly(t *testing.T) {
	ak := stubAccountKeeper{
		acc: accounttypes.Account{AccountIndex: 1, Collateral: math.NewInt(1_000)},
		pos: accounttypes.AccountPosition{
			AccountIndex: 1, MarketIndex: 0,
			BaseSize:                 math.NewInt(20),
			EntryQuote:               math.NewInt(19_500),
			LastFundingRatePrefixSum: math.ZeroInt(),
			AllocatedMargin:          math.ZeroInt(),
			MarginMode:               perptypes.CrossMargin,
		},
	}
	mk := stubMarketKeeper{md: markettypes.MarketDetails{
		DefaultInitialMarginFraction: 1000,
		MaintenanceMarginFraction:    500,
		CloseOutMarginFraction:       250,
	}}
	mk.md.MarkPrice = 1000
	k, ctx := makeKeeper(t, &ak, mk)

	pre, err := k.SnapshotRisk(ctx, 1)
	require.NoError(t, err)

	// Post: shrink position from 20 to 10. uPnL/collateral roughly
	// halves; TAV still > MMR; class stays at PRE or improves.
	ak.pos.BaseSize = math.NewInt(10)
	ak.pos.EntryQuote = math.NewInt(9_750)
	ok2, err := k.IsValidRiskChangeFrom(ctx, 1, pre)
	require.NoError(t, err)
	require.True(t, ok2, "shrinking position in PRE must be allowed")
}
