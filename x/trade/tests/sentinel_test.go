// sentinel_test.go pins the sentinel-error contract surface exposed
// by `x/trade/types` and consumed by the matching engine. Each test
// drives `ApplyPerpsMatching` / `ApplySpotMatching` into a specific
// rejection branch and asserts:
//
//  1. The returned error is the canonical `ErrTaker*` / `ErrMaker*`
//     sentinel (so `errors.Is` lookups in the matching engine match).
//  2. `IsRecoverableTakerError` / `IsRecoverableMakerError` route the
//     error to the correct side of the eviction-vs-stop fork.
//  3. Hard / unrelated errors do NOT classify as recoverable on
//     either side, so the matching loop falls through to a revert.
//
// Happy-path apply behaviour lives in perp_apply_test.go and
// spot_apply_test.go; isolated-margin allocation lives in
// isolated_margin_test.go.
package tests

import (
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

// TestApplyPerpsMatching_RejectsMakerRisk pins the invariant that the
// maker side is risk-checked alongside the taker; failing the maker
// risk check rejects the whole match.
func TestApplyPerpsMatching_RejectsMakerRisk(t *testing.T) {
	ctx, ak, _, rk, k := newSdkCtx(t)
	rk.rejectOnCall = 2 // first call = taker, second = maker

	require.NoError(t, ak.SetAccount(ctx, accounttypes.Account{AccountIndex: 10, Collateral: math.NewInt(1)}))
	require.NoError(t, ak.SetAccount(ctx, accounttypes.Account{AccountIndex: 20, Collateral: math.NewInt(1)}))

	err := k.ApplyPerpsMatching(ctx, tradekeeper.PerpFill{
		MakerAccountIndex: 10, TakerAccountIndex: 20,
		MarketIndex: 1, Price: 10, BaseAmount: 1,
		IsTakerAsk: false, NoFee: true,
	})
	require.Error(t, err)
	require.Contains(t, err.Error(), "risk regression")
	require.Equal(t, 2, rk.snapshots) // maker + taker both snapshotted
}

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
// maker enters with a position one base unit below the bit-width
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
		BaseSize:                 maxPos,
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
		BaseSize:                 maxPos,
		EntryQuote:               math.ZeroInt(),
		LastFundingRatePrefixSum: math.ZeroInt(),
		AllocatedMargin:          math.ZeroInt(),
		MarginMode:               perptypes.CrossMargin,
	}))
	// Maker has matching opposite-side capacity (a short position
	// big enough to absorb a +1 taker buy without itself overflowing).
	require.NoError(t, ak.SetPosition(ctx, accounttypes.AccountPosition{
		AccountIndex: makerIdx, MarketIndex: 1,
		BaseSize:                 math.NewInt(-1_000_000),
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
// the "maker has enough cross collateral" branch. The maker sits in
// isolated mode with zero allocated_margin; the fill grows
// |position| so `margin_delta > 0`. Set the stub's available cross
// collateral below `margin_delta` and ensure the trade is rejected
// with `ErrMakerInsufficientCollateral` before the post-trade risk
// check fires.
func TestSentinel_ApplyPerpsMatching_MakerInsufficientCollateral(t *testing.T) {
	ctx, ak, mk, rk, k := newSdkCtx(t)
	mk.markPrice = 100
	mk.imfBps = 1_000 // 10%
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
		BaseSize:                 math.ZeroInt(),
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
	ctx, ak, mk, rk, k := newSdkCtx(t)
	mk.markPrice = 100
	mk.imfBps = 1_000
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
		BaseSize:                 math.ZeroInt(),
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
