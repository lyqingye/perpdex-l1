// spot_apply_test.go covers the business behaviour of
// `tradekeeper.Keeper.ApplySpotMatching`: rejecting buys against a
// maker that lacks base balance (audit High trade-8) and the
// lock-on-place guarantee that successful fills drain the maker's
// locked balance before touching available balance.
//
// Sentinel error classification for spot insufficiency surfaces lives
// in sentinel_test.go.
package tests

import (
	"testing"

	"github.com/stretchr/testify/require"

	"cosmossdk.io/math"

	accounttypes "github.com/perpdex/perpdex-l1/x/account/types"
	tradekeeper "github.com/perpdex/perpdex-l1/x/trade/keeper"
)

// TestApplySpotMatching_RejectsNegativeBalance ensures a buy-side spot trade
// against a zero-balance maker errors instead of writing a negative balance
// (audit High trade-8).
func TestApplySpotMatching_RejectsNegativeBalance(t *testing.T) {
	ctx, _, _, _, k := newSdkCtx(t)

	// Taker buys from maker, but maker has no base balance.
	err := k.ApplySpotMatching(ctx, tradekeeper.SpotFill{
		MakerAccountIndex: 100, TakerAccountIndex: 200,
		MarketIndex: 2, Price: 5, BaseAmount: 10,
		IsTakerAsk: false, NoFee: true,
	}, uint32(1), uint32(3))
	require.Error(t, err)
	require.Contains(t, err.Error(), "insufficient balance")
}

// TestApplySpotMatching_DrainsLockedBalanceFirst confirms a successful
// spot fill consumes the maker's locked portion before touching the
// available balance — the lock-on-place guarantee.
func TestApplySpotMatching_DrainsLockedBalanceFirst(t *testing.T) {
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
