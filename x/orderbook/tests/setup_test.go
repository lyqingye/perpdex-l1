// Package tests hosts the external-package test suite for x/orderbook.
//
// setup_test.go gathers the fixtures shared across more than one test
// file: a minimal stub MarketKeeper that returns perps markets with
// zeroed details, a no-op SpotLocker (used by tests that do not
// exercise lock-on-place), the canonical perps keeper constructor, and
// the small Order factory used by the genesis and account-open-orders
// suites. Test-specific stubs (spot locker recorder, impact-tunable
// MarketKeeper, etc.) live alongside the tests that need them.
package tests

import (
	"context"
	"testing"

	cmtprototypes "github.com/cometbft/cometbft/proto/tendermint/types"

	"cosmossdk.io/log"
	cosmosmath "cosmossdk.io/math"
	storetypes "cosmossdk.io/store/types"

	"github.com/cosmos/cosmos-sdk/runtime"
	"github.com/cosmos/cosmos-sdk/testutil/integration"
	sdk "github.com/cosmos/cosmos-sdk/types"
	moduletestutil "github.com/cosmos/cosmos-sdk/types/module/testutil"

	perptypes "github.com/perpdex/perpdex-l1/types"
	markettypes "github.com/perpdex/perpdex-l1/x/market/types"
	orderbookkeeper "github.com/perpdex/perpdex-l1/x/orderbook/keeper"
	"github.com/perpdex/perpdex-l1/x/orderbook/types"
)

// stubMarketKeeper returns a perps market with zeroed details so the
// orderbook keeper never touches the lock path. Tests that need a spot
// market or a tunable MinIMF/QuoteMultiplier use their own stub.
type stubMarketKeeper struct{}

func (stubMarketKeeper) GetMarket(_ context.Context, idx uint32) (markettypes.Market, error) {
	return markettypes.Market{MarketIndex: idx, MarketType: perptypes.MarketTypePerps}, nil
}
func (stubMarketKeeper) GetMarketDetails(_ context.Context, idx uint32) (markettypes.MarketDetails, error) {
	return markettypes.MarketDetails{MarketIndex: idx}, nil
}
func (stubMarketKeeper) AllocateNonce(_ context.Context, _ uint32, _ bool) (int64, error) {
	return 1, nil
}
func (stubMarketKeeper) SetMarketDetails(_ context.Context, _ markettypes.MarketDetails) error {
	return nil
}

// stubLocker is a no-op SpotLocker used in orderbook unit tests that do
// not exercise lock-on-place behaviour.
type stubLocker struct{}

func (stubLocker) IncreaseLockedBalance(_ context.Context, _ uint64, _ uint32, _ cosmosmath.Int) error {
	return nil
}
func (stubLocker) DecreaseLockedBalance(_ context.Context, _ uint64, _ uint32, _ cosmosmath.Int) error {
	return nil
}

// newOrderbookKeeper wires the orderbook keeper against an in-memory
// multistore with the perps stub MarketKeeper and the no-op locker.
func newOrderbookKeeper(t *testing.T) (orderbookkeeper.Keeper, sdk.Context) {
	t.Helper()
	keys := storetypes.NewKVStoreKeys(types.StoreKey)
	cdc := moduletestutil.MakeTestEncodingConfig().Codec
	cms := integration.CreateMultiStore(keys, log.NewTestLogger(t))
	ctx := sdk.NewContext(cms, cmtprototypes.Header{}, true, log.NewTestLogger(t))
	k := orderbookkeeper.NewKeeper(
		cdc,
		runtime.NewKVStoreService(keys[types.StoreKey]),
		"px1xqcnyve5x5mrwwpev93xxer9venks6t29ke4l8",
		stubMarketKeeper{},
		stubLocker{},
	)
	return k, ctx
}

// makeOrder builds a minimal but valid Order suitable for OpenOrder. A
// non-zero RemainingBaseAmount and Price keep the underlying entry
// insert from rejecting on the quote-cap path.
func makeOrder(idx uint64, account uint64, market uint32, clientID uint64, isAsk bool) types.Order {
	return types.Order{
		OrderIndex:          idx,
		ClientOrderIndex:    clientID,
		OwnerAccountIndex:   account,
		MarketIndex:         market,
		IsAsk:               isAsk,
		OrderType:           perptypes.LimitOrder,
		TimeInForce:         perptypes.GTT,
		Price:               1000,
		Nonce:               int64(idx),
		InitialBaseAmount:   1,
		RemainingBaseAmount: 1,
		Status:              perptypes.OrderStatusOpen,
	}
}
