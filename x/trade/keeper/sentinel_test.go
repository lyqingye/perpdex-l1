package keeper_test

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/stretchr/testify/require"

	"cosmossdk.io/math"

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

	err := k.ApplyPerpsMatching(ctx, tradekeeper.Fill{
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

	err := k.ApplyPerpsMatching(ctx, tradekeeper.Fill{
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

	err := k.ApplySpotMatching(ctx, tradekeeper.Fill{
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

	err := k.ApplySpotMatching(ctx, tradekeeper.Fill{
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

	require.NoError(t, k.ApplySpotMatching(ctx, tradekeeper.Fill{
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

// helpKeepImports prevents unused-import lints when this file evolves.
var _ context.Context = nil
