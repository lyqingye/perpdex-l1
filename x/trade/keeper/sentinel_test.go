package keeper_test

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/stretchr/testify/require"

	"cosmossdk.io/math"

	perptypes "github.com/perpdex/perpdex-l1/types"
	accounttypes "github.com/perpdex/perpdex-l1/x/account/types"
	tradekeeper "github.com/perpdex/perpdex-l1/x/trade/keeper"
	tradetypes "github.com/perpdex/perpdex-l1/x/trade/types"
)

// TestSentinel_ApplyPerpsMatching_TakerRiskRegression confirms the taker
// branch of the risk loop returns ErrTakerRiskRegression so the matching
// engine can stop the taker but preserve previously committed fills.
func TestSentinel_ApplyPerpsMatching_TakerRiskRegression(t *testing.T) {
	ctx, ak, _, rk, k := newSdkCtx(t)
	rk.rejectOnCall = 1 // first call = taker

	require.NoError(t, ak.SetAccount(ctx, accounttypes.Account{AccountIndex: 10, Collateral: math.NewInt(1)}))
	require.NoError(t, ak.SetAccount(ctx, accounttypes.Account{AccountIndex: 20, Collateral: math.NewInt(1)}))

	err := k.ApplyPerpsMatching(ctx, tradekeeper.PerpFill{
		MakerAccountIndex: 10, TakerAccountIndex: 20,
		MarketIndex: 1, Price: 10, BaseAmount: 1,
		IsTakerAsk: false, NoFee: true,
	})
	require.Error(t, err)
	require.True(t, errors.Is(err, tradetypes.ErrTakerRiskRegression),
		"expected ErrTakerRiskRegression, got %v", err)
	require.True(t, tradetypes.IsRecoverableTakerError(err))
	require.False(t, tradetypes.IsRecoverableMakerError(err))
}

// TestSentinel_ApplyPerpsMatching_MakerRiskRegression confirms the maker
// branch returns ErrMakerRiskRegression — what the matching engine looks
// for to evict + continue.
func TestSentinel_ApplyPerpsMatching_MakerRiskRegression(t *testing.T) {
	ctx, ak, _, rk, k := newSdkCtx(t)
	rk.rejectOnCall = 2 // second call = maker

	require.NoError(t, ak.SetAccount(ctx, accounttypes.Account{AccountIndex: 10, Collateral: math.NewInt(1)}))
	require.NoError(t, ak.SetAccount(ctx, accounttypes.Account{AccountIndex: 20, Collateral: math.NewInt(1)}))

	err := k.ApplyPerpsMatching(ctx, tradekeeper.PerpFill{
		MakerAccountIndex: 10, TakerAccountIndex: 20,
		MarketIndex: 1, Price: 10, BaseAmount: 1,
		IsTakerAsk: false, NoFee: true,
	})
	require.Error(t, err)
	require.True(t, errors.Is(err, tradetypes.ErrMakerRiskRegression),
		"expected ErrMakerRiskRegression, got %v", err)
	require.True(t, tradetypes.IsRecoverableMakerError(err))
	require.False(t, tradetypes.IsRecoverableTakerError(err))
}

// TestSentinel_ApplySpotMatching_TakerInsufficient confirms the taker's
// available-balance shortfall surfaces as ErrTakerInsufficientBalance so
// the matching engine can stop the taker without reverting prior fills.
//
// Setup: maker has plenty of base; taker tries to buy but holds zero
// quote. IsTakerAsk = false ⇒ taker buys, owes quote, so the missing
// balance is on the TAKER side.
func TestSentinel_ApplySpotMatching_TakerInsufficient(t *testing.T) {
	ctx, ak, _, _, k := newSdkCtx(t)
	const (
		makerIdx     = uint64(10)
		takerIdx     = uint64(20)
		baseAssetID  = uint32(1)
		quoteAssetID = uint32(2)
	)
	// Maker holds 100 base, locked.
	require.NoError(t, ak.SetAccountAsset(ctx, accounttypes.AccountAsset{
		AccountIndex: makerIdx, AssetIndex: baseAssetID,
		Balance: math.NewInt(100), LockedBalance: math.NewInt(100),
	}))
	// Taker holds 0 quote.

	err := k.ApplySpotMatching(ctx, tradekeeper.SpotFill{
		MakerAccountIndex: makerIdx, TakerAccountIndex: takerIdx,
		MarketIndex: 1, Price: 5, BaseAmount: 10,
		IsTakerAsk: false, NoFee: true,
	}, baseAssetID, quoteAssetID)
	require.Error(t, err)
	require.True(t, errors.Is(err, tradetypes.ErrTakerInsufficientBalance),
		"expected ErrTakerInsufficientBalance, got %v", err)
	require.True(t, tradetypes.IsRecoverableTakerError(err))
}

// TestSentinel_ApplySpotMatching_MakerInsufficient confirms the maker's
// shortfall surfaces as ErrMakerInsufficientBalance so the matching loop
// can evict + continue. Setup: taker holds enough quote; maker has zero
// base — buying side fails on maker.
func TestSentinel_ApplySpotMatching_MakerInsufficient(t *testing.T) {
	ctx, ak, _, _, k := newSdkCtx(t)
	const (
		makerIdx     = uint64(10)
		takerIdx     = uint64(20)
		baseAssetID  = uint32(1)
		quoteAssetID = uint32(2)
	)
	// Taker holds 1000 quote (Available = 1000, no locks).
	require.NoError(t, ak.SetAccountAsset(ctx, accounttypes.AccountAsset{
		AccountIndex: takerIdx, AssetIndex: quoteAssetID,
		Balance: math.NewInt(1000),
	}))
	// Maker holds 0 base.

	err := k.ApplySpotMatching(ctx, tradekeeper.SpotFill{
		MakerAccountIndex: makerIdx, TakerAccountIndex: takerIdx,
		MarketIndex: 1, Price: 5, BaseAmount: 10,
		IsTakerAsk: false, NoFee: true,
	}, baseAssetID, quoteAssetID)
	require.Error(t, err)
	require.True(t, errors.Is(err, tradetypes.ErrMakerInsufficientBalance),
		"expected ErrMakerInsufficientBalance, got %v", err)
	require.True(t, tradetypes.IsRecoverableMakerError(err))
}

// TestSentinel_ApplySpotMatching_DrainsLockedBalanceFirst confirms a
// successful spot fill consumes the maker's locked portion before
// touching the available balance — the lock-on-place guarantee.
func TestSentinel_ApplySpotMatching_DrainsLockedBalanceFirst(t *testing.T) {
	ctx, ak, _, _, k := newSdkCtx(t)
	const (
		makerIdx     = uint64(10)
		takerIdx     = uint64(20)
		baseAssetID  = uint32(1)
		quoteAssetID = uint32(2)
	)
	// Maker rests an ask: 50 base locked, total balance 100.
	require.NoError(t, ak.SetAccountAsset(ctx, accounttypes.AccountAsset{
		AccountIndex: makerIdx, AssetIndex: baseAssetID,
		Balance: math.NewInt(100), LockedBalance: math.NewInt(50),
	}))
	// Taker has plenty of quote.
	require.NoError(t, ak.SetAccountAsset(ctx, accounttypes.AccountAsset{
		AccountIndex: takerIdx, AssetIndex: quoteAssetID,
		Balance: math.NewInt(10_000),
	}))

	require.NoError(t, k.ApplySpotMatching(ctx, tradekeeper.SpotFill{
		MakerAccountIndex: makerIdx, TakerAccountIndex: takerIdx,
		MarketIndex: 1, Price: 1, BaseAmount: 30,
		IsTakerAsk: false, NoFee: true,
	}, baseAssetID, quoteAssetID))

	mAsset, err := ak.GetAccountAsset(ctx, makerIdx, baseAssetID)
	require.NoError(t, err)
	require.Equal(t, "70", mAsset.Balance.String())
	// Lock drained by 30 (the fill size).
	require.Equal(t, "20", mAsset.LockedBalance.String(),
		"locked balance must drain by fill amount before touching available")
}

// TestSentinel_HardErrorClassification confirms that non-sentinel errors
// (raw fmt.Errorf or unrelated errors.New) are reported as neither maker
// nor taker recoverable so the matching loop falls through to the hard-
// revert branch.
func TestSentinel_HardErrorClassification(t *testing.T) {
	hard := fmt.Errorf("simulated db corruption")
	require.False(t, tradetypes.IsRecoverableMakerError(hard))
	require.False(t, tradetypes.IsRecoverableTakerError(hard))

	// Nil should also be safely classified as not-recoverable.
	require.False(t, tradetypes.IsRecoverableMakerError(nil))
	require.False(t, tradetypes.IsRecoverableTakerError(nil))
}

// TestSentinel_ApplyPerpsMatching_MakerInvalidPosition triggers the
// post-trade `|position|` overflow branch on the maker side. The
// maker enters with a position one base unit below the prover circuit
// limit (POSITION_SIZE_BITS = 56) and the taker sells one base unit
// against it — a buy from the maker grows |position| past the bound.
// Result must be ErrMakerInvalidPosition + recoverable so the matching
// loop evicts the maker.
func TestSentinel_ApplyPerpsMatching_MakerInvalidPosition(t *testing.T) {
	ctx, ak, _, _, k := newSdkCtx(t)

	const (
		makerIdx = uint64(10)
		takerIdx = uint64(20)
	)
	require.NoError(t, ak.SetAccount(ctx, accounttypes.Account{AccountIndex: makerIdx, Collateral: math.NewInt(1_000_000_000)}))
	require.NoError(t, ak.SetAccount(ctx, accounttypes.Account{AccountIndex: takerIdx, Collateral: math.NewInt(1_000_000_000)}))
	// Prime maker right at the limit (already-MAX position): the taker
	// is selling, IsTakerAsk=true ⇒ taker -1, maker +1, so maker grows
	// past `MaxPositionSize`.
	maxPos := math.NewIntFromUint64(perptypes.MaxPositionSize)
	require.NoError(t, ak.SetPosition(ctx, accounttypes.AccountPosition{
		AccountIndex: makerIdx, MarketIndex: 1,
		Size_:                    maxPos,
		EntryQuote:               math.ZeroInt(),
		LastFundingRatePrefixSum: math.ZeroInt(),
		AllocatedMargin:          math.ZeroInt(),
		MarginMode:               perptypes.CrossMargin,
	}))

	err := k.ApplyPerpsMatching(ctx, tradekeeper.PerpFill{
		MakerAccountIndex: makerIdx, TakerAccountIndex: takerIdx,
		MarketIndex: 1, Price: 1, BaseAmount: 1,
		IsTakerAsk: true, NoFee: true,
	})
	require.Error(t, err)
	require.True(t, errors.Is(err, tradetypes.ErrMakerInvalidPosition),
		"expected ErrMakerInvalidPosition, got %v", err)
	require.True(t, tradetypes.IsRecoverableMakerError(err))
	require.False(t, tradetypes.IsRecoverableTakerError(err))
}

// TestSentinel_ApplyPerpsMatching_TakerInvalidPosition is the symmetric
// version: the taker enters at MaxPositionSize and the trade grows it
// further. Result must be ErrTakerInvalidPosition + recoverable taker.
func TestSentinel_ApplyPerpsMatching_TakerInvalidPosition(t *testing.T) {
	ctx, ak, _, _, k := newSdkCtx(t)

	const (
		makerIdx = uint64(10)
		takerIdx = uint64(20)
	)
	require.NoError(t, ak.SetAccount(ctx, accounttypes.Account{AccountIndex: makerIdx, Collateral: math.NewInt(1_000_000_000)}))
	require.NoError(t, ak.SetAccount(ctx, accounttypes.Account{AccountIndex: takerIdx, Collateral: math.NewInt(1_000_000_000)}))
	// Taker is buying (IsTakerAsk=false ⇒ taker +1). Pre-load taker
	// at MaxPositionSize so the trade overflows on the taker side
	// before it reaches the maker side's bounds check.
	maxPos := math.NewIntFromUint64(perptypes.MaxPositionSize)
	require.NoError(t, ak.SetPosition(ctx, accounttypes.AccountPosition{
		AccountIndex: takerIdx, MarketIndex: 1,
		Size_:                    maxPos,
		EntryQuote:               math.ZeroInt(),
		LastFundingRatePrefixSum: math.ZeroInt(),
		AllocatedMargin:          math.ZeroInt(),
		MarginMode:               perptypes.CrossMargin,
	}))
	// Maker has matching opposite-side capacity (a short position
	// big enough to absorb a +1 taker buy without itself overflowing).
	require.NoError(t, ak.SetPosition(ctx, accounttypes.AccountPosition{
		AccountIndex: makerIdx, MarketIndex: 1,
		Size_:                    math.NewInt(-1_000_000),
		EntryQuote:               math.ZeroInt(),
		LastFundingRatePrefixSum: math.ZeroInt(),
		AllocatedMargin:          math.ZeroInt(),
		MarginMode:               perptypes.CrossMargin,
	}))

	err := k.ApplyPerpsMatching(ctx, tradekeeper.PerpFill{
		MakerAccountIndex: makerIdx, TakerAccountIndex: takerIdx,
		MarketIndex: 1, Price: 1, BaseAmount: 1,
		IsTakerAsk: false, NoFee: true,
	})
	require.Error(t, err)
	require.True(t, errors.Is(err, tradetypes.ErrTakerInvalidPosition),
		"expected ErrTakerInvalidPosition, got %v", err)
	require.True(t, tradetypes.IsRecoverableTakerError(err))
	require.False(t, tradetypes.IsRecoverableMakerError(err))
}

// TestSentinel_ApplyPerpsMatching_MakerInsufficientCollateral exercises
// the lighter `is_maker_has_enough_cross_collateral` branch. The maker
// sits in isolated mode with zero allocated_margin; the fill grows
// |position| so `margin_delta > 0`. Set the stub's available cross
// collateral below `margin_delta` and ensure the trade is rejected
// with `ErrMakerInsufficientCollateral` before the post-trade risk
// check fires.
func TestSentinel_ApplyPerpsMatching_MakerInsufficientCollateral(t *testing.T) {
	ctx, ak, _, rk, k := newSdkCtx(t)
	rk.markPrice = 100
	rk.imfBps = 1_000 // 10%
	rk.availableCollateral = map[uint64]math.Int{
		10: math.NewInt(1), // far less than the IM the fill demands
	}

	const (
		makerIdx = uint64(10)
		takerIdx = uint64(20)
	)
	require.NoError(t, ak.SetAccount(ctx, accounttypes.Account{AccountIndex: makerIdx, Collateral: math.NewInt(1_000_000)}))
	require.NoError(t, ak.SetAccount(ctx, accounttypes.Account{AccountIndex: takerIdx, Collateral: math.NewInt(1_000_000)}))
	require.NoError(t, ak.SetPosition(ctx, accounttypes.AccountPosition{
		AccountIndex: makerIdx, MarketIndex: 1,
		Size_:                    math.ZeroInt(),
		EntryQuote:               math.ZeroInt(),
		LastFundingRatePrefixSum: math.ZeroInt(),
		AllocatedMargin:          math.ZeroInt(),
		MarginMode:               perptypes.IsolatedMargin,
	}))

	err := k.ApplyPerpsMatching(ctx, tradekeeper.PerpFill{
		MakerAccountIndex: makerIdx, TakerAccountIndex: takerIdx,
		MarketIndex: 1, Price: 100, BaseAmount: 10,
		IsTakerAsk: false, NoFee: true,
	})
	require.Error(t, err)
	require.True(t, errors.Is(err, tradetypes.ErrMakerInsufficientCollateral),
		"expected ErrMakerInsufficientCollateral, got %v", err)
	require.True(t, tradetypes.IsRecoverableMakerError(err))
	require.False(t, tradetypes.IsRecoverableTakerError(err))
}

// TestSentinel_ApplyPerpsMatching_TakerInsufficientCollateral is the
// symmetric taker variant.
func TestSentinel_ApplyPerpsMatching_TakerInsufficientCollateral(t *testing.T) {
	ctx, ak, _, rk, k := newSdkCtx(t)
	rk.markPrice = 100
	rk.imfBps = 1_000
	rk.availableCollateral = map[uint64]math.Int{
		20: math.NewInt(1),
	}

	const (
		makerIdx = uint64(10)
		takerIdx = uint64(20)
	)
	require.NoError(t, ak.SetAccount(ctx, accounttypes.Account{AccountIndex: makerIdx, Collateral: math.NewInt(1_000_000)}))
	require.NoError(t, ak.SetAccount(ctx, accounttypes.Account{AccountIndex: takerIdx, Collateral: math.NewInt(1_000_000)}))
	require.NoError(t, ak.SetPosition(ctx, accounttypes.AccountPosition{
		AccountIndex: takerIdx, MarketIndex: 1,
		Size_:                    math.ZeroInt(),
		EntryQuote:               math.ZeroInt(),
		LastFundingRatePrefixSum: math.ZeroInt(),
		AllocatedMargin:          math.ZeroInt(),
		MarginMode:               perptypes.IsolatedMargin,
	}))

	err := k.ApplyPerpsMatching(ctx, tradekeeper.PerpFill{
		MakerAccountIndex: makerIdx, TakerAccountIndex: takerIdx,
		MarketIndex: 1, Price: 100, BaseAmount: 10,
		IsTakerAsk: false, NoFee: true,
	})
	require.Error(t, err)
	require.True(t, errors.Is(err, tradetypes.ErrTakerInsufficientCollateral),
		"expected ErrTakerInsufficientCollateral, got %v", err)
	require.True(t, tradetypes.IsRecoverableTakerError(err))
	require.False(t, tradetypes.IsRecoverableMakerError(err))
}

// TestSentinel_IsolatedMarginDelta_OpenIncreasesAllocation confirms the
// case-3 (OI grew, side same) branch: when an isolated position grows
// and there is no incremental PnL, allocated_margin must rise by the
// IM of the OI delta and an equal amount must be debited from cross
// collateral.
func TestSentinel_IsolatedMarginDelta_OpenIncreasesAllocation(t *testing.T) {
	ctx, ak, _, rk, k := newSdkCtx(t)
	rk.markPrice = 100
	rk.imfBps = 1_000 // 10%

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
		Size_:                    math.ZeroInt(),
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
	ctx, ak, _, rk, k := newSdkCtx(t)
	rk.markPrice = 100
	rk.imfBps = 1_000

	const (
		makerIdx = uint64(10)
		takerIdx = uint64(20)
	)
	require.NoError(t, ak.SetAccount(ctx, accounttypes.Account{AccountIndex: makerIdx, Collateral: math.NewInt(1_000_000)}))
	require.NoError(t, ak.SetAccount(ctx, accounttypes.Account{AccountIndex: takerIdx, Collateral: math.NewInt(0)}))
	// Pre-load taker as a long isolated position with allocated 100.
	require.NoError(t, ak.SetPosition(ctx, accounttypes.AccountPosition{
		AccountIndex: takerIdx, MarketIndex: 1,
		Size_:                    math.NewInt(10),
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
	require.True(t, pos.Size_.IsZero(), "position must close")
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
	ctx, ak, _, rk, k := newSdkCtx(t)
	rk.markPrice = 100
	rk.imfBps = 1_000

	const (
		makerIdx = uint64(10)
		takerIdx = uint64(20)
	)
	require.NoError(t, ak.SetAccount(ctx, accounttypes.Account{AccountIndex: makerIdx, Collateral: math.NewInt(1_000_000)}))
	require.NoError(t, ak.SetAccount(ctx, accounttypes.Account{AccountIndex: takerIdx, Collateral: math.NewInt(0)}))
	require.NoError(t, ak.SetPosition(ctx, accounttypes.AccountPosition{
		AccountIndex: takerIdx, MarketIndex: 1,
		Size_:                    math.NewInt(10),
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
	require.Equal(t, "5", pos.Size_.String())
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
	ctx, ak, _, rk, k := newSdkCtx(t)
	rk.markPrice = 100
	rk.imfBps = 1_000

	const (
		makerIdx = uint64(10)
		takerIdx = uint64(20)
	)
	require.NoError(t, ak.SetAccount(ctx, accounttypes.Account{AccountIndex: makerIdx, Collateral: math.NewInt(1_000_000)}))
	require.NoError(t, ak.SetAccount(ctx, accounttypes.Account{AccountIndex: takerIdx, Collateral: math.NewInt(0)}))
	require.NoError(t, ak.SetPosition(ctx, accounttypes.AccountPosition{
		AccountIndex: takerIdx, MarketIndex: 1,
		Size_:                    math.NewInt(5),
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
	require.Equal(t, "-5", pos.Size_.String())
	require.Equal(t, "50", pos.AllocatedMargin.String(),
		"flip with zero PnL should leave allocated == position_requirement")
	acc, err := ak.GetAccount(ctx, takerIdx)
	require.NoError(t, err)
	require.True(t, acc.Collateral.IsZero(),
		"flip with neutral margin_delta must not move cross collateral")
}

// helpKeepImports prevents unused-import lints when this file evolves.
var _ context.Context = nil
var _ = perptypes.MaxPositionSize
