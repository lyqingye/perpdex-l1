// liquidation_test.go covers liquidation-side reads on the risk
// keeper: the mark-based zero-price formula
// (riskkeeper.GetPositionZeroPrice) for long / short / isolated
// positions, and the empty-position short-circuit on
// GetLiquidationRiskSnapshot / GetPositionZeroPrice that lets
// callers (gRPC, liquidation, deleverage) safely query risk for
// accounts that have no exposure.
package tests

import (
	"testing"

	"cosmossdk.io/math"
	"github.com/stretchr/testify/require"

	perptypes "github.com/perpdex/perpdex-l1/types"
	accounttypes "github.com/perpdex/perpdex-l1/x/account/types"
	markettypes "github.com/perpdex/perpdex-l1/x/market/types"
)

// TestGetPositionZeroPrice_LongMarkBased verifies the new mark-based
// zero-price formula for a long position. With M_i = 1% (100 bps),
// TAV = 50, MMR = 100, mark = 1000:
//
//	zeroPrice = mark * (1 - M_i * TAV / MMR)
//	          = 1000 * (1 - 0.01 * 50 / 100)
//	          = 1000 * (1 - 0.005) = 995
func TestGetPositionZeroPrice_LongMarkBased(t *testing.T) {
	ak := stubAccountKeeper{
		acc: accounttypes.Account{AccountIndex: 1, Collateral: math.NewInt(40)},
		pos: accounttypes.AccountPosition{
			AccountIndex: 1, MarketIndex: 0,
			BaseSize:                 math.NewInt(10), // long
			EntryQuote:               math.NewInt(-9_900),
			LastFundingRatePrefixSum: math.ZeroInt(),
			AllocatedMargin:          math.ZeroInt(),
			MarginMode:               perptypes.CrossMargin,
		},
	}
	// IM/MM/CM = 1% / 1% / 0.5%, mark = 1000.
	// notional = |10| * 1000 = 10_000, MMR = 10_000 * 0.01 = 100.
	// uPnL = 10*1000 - (-9_900) = 19_900? That's wrong; we want a
	// small TAV so the formula is intuitive. Adjust collateral so:
	// TAV = collateral + uPnL = 40 + (10_000 - 9_900) = 140? No, uPnL
	// = pos*mark - entryQuote = 10*1000 - (-9_900) = 19_900.
	// Actually entryQuote semantics: a long with cost basis 9_900 has
	// EntryQuote = -9_900 (paid quote out). So uPnL = pos*mark -
	// EntryQuote = 10_000 - (-9_900) = 19_900. We want TAV=50, so
	// reset entryQuote.
	// mark*pos - entry = 50 - collateral = 50 - 40 = 10  ⇒ entry =
	// 10_000 - 10 = 9_990.
	ak.pos.EntryQuote = math.NewInt(9_990)
	mk := stubMarketKeeper{md: markettypes.MarketDetails{
		DefaultInitialMarginFraction: 100, // 1%
		MaintenanceMarginFraction:    100, // 1%
		CloseOutMarginFraction:       50,  // 0.5%
	}}
	mk.md.MarkPrice = 1000
	k, ctx := makeKeeper(t, &ak, mk)

	zp, err := k.GetPositionZeroPrice(ctx, 1, 0)
	require.NoError(t, err)
	// mark = 1000, M_i = 100/10_000 = 0.01, TAV = 50, MMR = 100.
	// adjustment = 1000 * 100 * 50 / (100 * 10_000) = 5.
	// zeroPrice = 1000 - 5 = 995.
	require.Equal(t, uint32(995), zp,
		"long zero price must be mark*(1 - M*TAV/MMR)")
}

// TestGetPositionZeroPrice_ShortMarkBased mirrors the long test for a
// short position; the adjustment must ADD instead of subtract.
func TestGetPositionZeroPrice_ShortMarkBased(t *testing.T) {
	ak := stubAccountKeeper{
		acc: accounttypes.Account{AccountIndex: 1, Collateral: math.NewInt(40)},
		pos: accounttypes.AccountPosition{
			AccountIndex: 1, MarketIndex: 0,
			BaseSize:                 math.NewInt(-10), // short
			EntryQuote:               math.NewInt(-10_010),
			LastFundingRatePrefixSum: math.ZeroInt(),
			AllocatedMargin:          math.ZeroInt(),
			MarginMode:               perptypes.CrossMargin,
		},
	}
	mk := stubMarketKeeper{md: markettypes.MarketDetails{
		DefaultInitialMarginFraction: 100,
		MaintenanceMarginFraction:    100,
		CloseOutMarginFraction:       50,
	}}
	mk.md.MarkPrice = 1000
	k, ctx := makeKeeper(t, &ak, mk)

	zp, err := k.GetPositionZeroPrice(ctx, 1, 0)
	require.NoError(t, err)
	// uPnL = -10*1000 - (-10_010) = 10. TAV = 40 + 10 = 50? Wait —
	// collateral=40, uPnL=10, TAV=50, MMR=|−10|*1000*0.01 = 100.
	// adjustment = 1000 * 100 * 50 / (100 * 10_000) = 5.
	// zeroPrice short = mark + adjustment = 1005.
	require.Equal(t, uint32(1005), zp,
		"short zero price must be mark*(1 + M*TAV/MMR)")
}

// TestGetPositionZeroPrice_IsolatedUsesIsolatedTAV ensures the formula
// uses the isolated position's AllocatedMargin + uPnL (not cross
// aggregates) when the position is isolated.
func TestGetPositionZeroPrice_IsolatedUsesIsolatedTAV(t *testing.T) {
	ak := stubAccountKeeper{
		acc: accounttypes.Account{AccountIndex: 1, Collateral: math.NewInt(1_000_000)},
		pos: accounttypes.AccountPosition{
			AccountIndex: 1, MarketIndex: 0,
			BaseSize:                 math.NewInt(10),
			EntryQuote:               math.NewInt(9_990), // uPnL = 10
			LastFundingRatePrefixSum: math.ZeroInt(),
			AllocatedMargin:          math.NewInt(40), // isolated TAV = 50
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

	zp, err := k.GetPositionZeroPrice(ctx, 1, 0)
	require.NoError(t, err)
	// TAV = AllocatedMargin + uPnL = 40 + 10 = 50; MMR = 100.
	// adjustment = 5 ⇒ zeroPrice = 1000 - 5 = 995.
	require.Equal(t, uint32(995), zp,
		"isolated zero price must use AllocatedMargin + uPnL, NOT cross collateral")
}

// TestGetLiquidationRiskSnapshot_EmptyPosition pins the invariant
// that a snapshot for an empty position short-circuits to a zero
// snapshot without reading mark price — so a missing or zero mark
// for an account with no exposure cannot surface as an error to
// callers (gRPC GetPositionZeroPrice, Liquidate, Deleverage).
func TestGetLiquidationRiskSnapshot_EmptyPosition(t *testing.T) {
	ak := stubAccountKeeper{
		acc: accounttypes.Account{AccountIndex: 1, Collateral: math.NewInt(1_000)},
		pos: accounttypes.AccountPosition{
			AccountIndex: 1, MarketIndex: 99, // a different market
			BaseSize: math.ZeroInt(), EntryQuote: math.ZeroInt(),
			LastFundingRatePrefixSum: math.ZeroInt(), AllocatedMargin: math.ZeroInt(),
		},
	}
	// mk.md.MarkPrice == 0: the mark gate would fail; the short-circuit
	// must run before that.
	mk := stubMarketKeeper{md: markettypes.MarketDetails{}}
	k, ctx := makeKeeper(t, &ak, mk)

	snap, err := k.GetLiquidationRiskSnapshot(ctx, 1, 0)
	require.NoError(t, err, "empty position must short-circuit before any mark read")
	require.True(t, snap.Position.BaseSize.IsZero())
	require.Equal(t, uint32(0), snap.MarkPrice)
	require.Equal(t, uint32(0), snap.ZeroPrice)

	zp, err := k.GetPositionZeroPrice(ctx, 1, 0)
	require.NoError(t, err, "GetPositionZeroPrice must short-circuit on empty position")
	require.Equal(t, uint32(0), zp)
}
