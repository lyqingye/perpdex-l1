// isolated_margin_test.go exercises the four-case isolated-margin
// allocation ladder that `ApplyPerpsMatching` drives on positions in
// `IsolatedMargin` mode:
//
//   - case-1: position closed → release all allocated_margin to cross
//   - case-2: position flipped sign → re-margin to position_requirement
//   - case-3: |position| grew, same side → top up allocated_margin by IM(Δ)
//   - case-4: |position| shrank, same side → release proportional excess
//
// Each test asserts the post-trade `allocated_margin` AND the
// account-level cross collateral so the margin transfer is observed
// end-to-end, not just on the position record.
package tests

import (
	"testing"

	"github.com/stretchr/testify/require"

	"cosmossdk.io/math"

	perptypes "github.com/perpdex/perpdex-l1/types"
	accounttypes "github.com/perpdex/perpdex-l1/x/account/types"
	tradekeeper "github.com/perpdex/perpdex-l1/x/trade/keeper"
)

// TestSentinel_IsolatedMarginDelta_OpenIncreasesAllocation confirms the
// case-3 (OI grew, side same) branch: when an isolated position grows
// and there is no incremental PnL, allocated_margin must rise by the
// IM of the OI delta and an equal amount must be debited from cross
// collateral.
func TestSentinel_IsolatedMarginDelta_OpenIncreasesAllocation(t *testing.T) {
	ctx, ak, mk, _, k := newSdkCtx(t)
	mk.markPrice = 100
	mk.imfBps = 1_000 // 10%

	const (
		makerIdx = uint64(10)
		takerIdx = uint64(20)
	)
	// Start with healthy cross collateral on the taker (the side we
	// observe). Maker is left in cross mode so we don't conflate
	// effects.
	require.NoError(t, ak.SetAccount(ctx, accounttypes.Account{AccountIndex: makerIdx, Collateral: math.NewInt(1_000_000)}))
	require.NoError(t, ak.SetAccount(ctx, accounttypes.Account{AccountIndex: takerIdx, Collateral: math.NewInt(1_000_000)}))
	require.NoError(t, ak.SetPosition(ctx, accounttypes.AccountPosition{
		AccountIndex: takerIdx, MarketIndex: 1,
		BaseSize:                 math.ZeroInt(),
		EntryQuote:               math.ZeroInt(),
		LastFundingRatePrefixSum: math.ZeroInt(),
		AllocatedMargin:          math.ZeroInt(),
		MarginMode:               perptypes.IsolatedMargin,
	}))

	require.NoError(t, k.ApplyPerpsMatching(ctx, tradekeeper.PerpFill{
		MakerAccountIndex: makerIdx, TakerAccountIndex: takerIdx,
		MarketIndex: 1, Price: 100, BaseAmount: 10,
		IsTakerAsk: false, NoFee: true,
	}))

	pos, err := ak.GetPosition(ctx, takerIdx, 1)
	require.NoError(t, err)
	// IM = |new| * mark * imfBps / MarginTick = 10 * 100 * 1000 / 10000 = 100.
	require.Equal(t, "100", pos.AllocatedMargin.String(),
		"isolated open should pull IM into allocated_margin")
	acc, err := ak.GetAccount(ctx, takerIdx)
	require.NoError(t, err)
	// Cross collateral starts at 1_000_000 and is debited by 100.
	require.Equal(t, "999900", acc.Collateral.String(),
		"cross collateral must drop by margin_delta")
}

// TestSentinel_IsolatedMarginDelta_CloseReleasesAllocation confirms the
// case-1 (closed) branch: closing an isolated position releases the
// remaining allocated_margin back to cross collateral.
func TestSentinel_IsolatedMarginDelta_CloseReleasesAllocation(t *testing.T) {
	ctx, ak, mk, _, k := newSdkCtx(t)
	mk.markPrice = 100
	mk.imfBps = 1_000

	const (
		makerIdx = uint64(10)
		takerIdx = uint64(20)
	)
	require.NoError(t, ak.SetAccount(ctx, accounttypes.Account{AccountIndex: makerIdx, Collateral: math.NewInt(1_000_000)}))
	require.NoError(t, ak.SetAccount(ctx, accounttypes.Account{AccountIndex: takerIdx, Collateral: math.NewInt(0)}))
	// Pre-load taker as a long isolated position with allocated 100.
	require.NoError(t, ak.SetPosition(ctx, accounttypes.AccountPosition{
		AccountIndex: takerIdx, MarketIndex: 1,
		BaseSize:                 math.NewInt(10),
		EntryQuote:               math.NewInt(10 * 100), // entry = 100
		LastFundingRatePrefixSum: math.ZeroInt(),
		AllocatedMargin:          math.NewInt(100),
		MarginMode:               perptypes.IsolatedMargin,
	}))

	// Taker sells 10 at the SAME price → no PnL.
	require.NoError(t, k.ApplyPerpsMatching(ctx, tradekeeper.PerpFill{
		MakerAccountIndex: makerIdx, TakerAccountIndex: takerIdx,
		MarketIndex: 1, Price: 100, BaseAmount: 10,
		IsTakerAsk: true, NoFee: true,
	}))

	pos, err := ak.GetPosition(ctx, takerIdx, 1)
	require.NoError(t, err)
	require.True(t, pos.BaseSize.IsZero(), "position must close")
	require.True(t, pos.AllocatedMargin.IsZero(),
		"all allocated_margin must release on close")
	acc, err := ak.GetAccount(ctx, takerIdx)
	require.NoError(t, err)
	require.Equal(t, "100", acc.Collateral.String(),
		"released allocated_margin must land on cross collateral")
}

// TestSentinel_IsolatedMarginDelta_DecreaseProportional exercises the
// case-4 (OI shrank, side same) branch: a partial close should release
// the proportional excess back to cross. Setup: long 10 at entry 100
// with allocated 200 (over-margined). Sell 5 at mark 100 → no PnL,
// new size 5, IM = 5*100*0.1 = 50. Old market value = 200, ceil_div
// (200*5,10) = 100; max(100, 50) = 100 = target. New market value =
// 200 + 0 = 200 (allocated unchanged so far). excess = 200 - 100 =
// 100. Move 100 back to cross.
func TestSentinel_IsolatedMarginDelta_DecreaseProportional(t *testing.T) {
	ctx, ak, mk, _, k := newSdkCtx(t)
	mk.markPrice = 100
	mk.imfBps = 1_000

	const (
		makerIdx = uint64(10)
		takerIdx = uint64(20)
	)
	require.NoError(t, ak.SetAccount(ctx, accounttypes.Account{AccountIndex: makerIdx, Collateral: math.NewInt(1_000_000)}))
	require.NoError(t, ak.SetAccount(ctx, accounttypes.Account{AccountIndex: takerIdx, Collateral: math.NewInt(0)}))
	require.NoError(t, ak.SetPosition(ctx, accounttypes.AccountPosition{
		AccountIndex: takerIdx, MarketIndex: 1,
		BaseSize:                 math.NewInt(10),
		EntryQuote:               math.NewInt(1000),
		LastFundingRatePrefixSum: math.ZeroInt(),
		AllocatedMargin:          math.NewInt(200),
		MarginMode:               perptypes.IsolatedMargin,
	}))

	require.NoError(t, k.ApplyPerpsMatching(ctx, tradekeeper.PerpFill{
		MakerAccountIndex: makerIdx, TakerAccountIndex: takerIdx,
		MarketIndex: 1, Price: 100, BaseAmount: 5,
		IsTakerAsk: true, NoFee: true,
	}))

	pos, err := ak.GetPosition(ctx, takerIdx, 1)
	require.NoError(t, err)
	require.Equal(t, "5", pos.BaseSize.String())
	require.Equal(t, "100", pos.AllocatedMargin.String(),
		"50%% size reduction with no PnL should halve allocated_margin")
	acc, err := ak.GetAccount(ctx, takerIdx)
	require.NoError(t, err)
	require.Equal(t, "100", acc.Collateral.String(),
		"released excess must flow back to cross")
}

// TestSentinel_IsolatedMarginDelta_FlipReMarginsToPositionRequirement
// exercises the case-2 (side flipped) branch. Long 5 at entry 100,
// allocated 50, mark 100 → uPnL = 0. Sell 10 at mark 100 → realized
// PnL = (close 5) -100*5 + 500 = 0; new position = -5; new entry
// price set by `applyPositionChange.flip` branch = newSize *
// price = -5 * 100 = -500 (so uPnL_new = -5*100 - (-500) = 0).
// position_requirement = 5*100*0.1 = 50. delta = 50 -
// (allocated_after_pnl + uPnL_new) = 50 - 50 = 0. allocated stays
// at 50.
func TestSentinel_IsolatedMarginDelta_FlipReMarginsToPositionRequirement(t *testing.T) {
	ctx, ak, mk, _, k := newSdkCtx(t)
	mk.markPrice = 100
	mk.imfBps = 1_000

	const (
		makerIdx = uint64(10)
		takerIdx = uint64(20)
	)
	require.NoError(t, ak.SetAccount(ctx, accounttypes.Account{AccountIndex: makerIdx, Collateral: math.NewInt(1_000_000)}))
	require.NoError(t, ak.SetAccount(ctx, accounttypes.Account{AccountIndex: takerIdx, Collateral: math.NewInt(0)}))
	require.NoError(t, ak.SetPosition(ctx, accounttypes.AccountPosition{
		AccountIndex: takerIdx, MarketIndex: 1,
		BaseSize:                 math.NewInt(5),
		EntryQuote:               math.NewInt(500),
		LastFundingRatePrefixSum: math.ZeroInt(),
		AllocatedMargin:          math.NewInt(50),
		MarginMode:               perptypes.IsolatedMargin,
	}))

	require.NoError(t, k.ApplyPerpsMatching(ctx, tradekeeper.PerpFill{
		MakerAccountIndex: makerIdx, TakerAccountIndex: takerIdx,
		MarketIndex: 1, Price: 100, BaseAmount: 10,
		IsTakerAsk: true, NoFee: true,
	}))

	pos, err := ak.GetPosition(ctx, takerIdx, 1)
	require.NoError(t, err)
	require.Equal(t, "-5", pos.BaseSize.String())
	require.Equal(t, "50", pos.AllocatedMargin.String(),
		"flip with zero PnL should leave allocated == position_requirement")
	acc, err := ak.GetAccount(ctx, takerIdx)
	require.NoError(t, err)
	require.True(t, acc.Collateral.IsZero(),
		"flip with neutral margin_delta must not move cross collateral")
}
