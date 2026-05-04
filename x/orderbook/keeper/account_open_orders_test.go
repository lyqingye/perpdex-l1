package keeper_test

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"

	cmtprototypes "github.com/cometbft/cometbft/proto/tendermint/types"

	"cosmossdk.io/log"
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
	)
	return k, ctx
}

// TestAccountOpenOrders_CoversNoClientID confirms that orders without a
// client_order_index (the optional field defaulting to 0) are still
// reachable via the AccountOpenOrders index, so cancel-all can find them.
func TestAccountOpenOrders_CoversNoClientID(t *testing.T) {
	k, ctx := newOrderbookKeeper(t)

	// Order A: with client_order_index
	a := types.Order{
		OrderIndex:        1,
		ClientOrderIndex:  10,
		OwnerAccountIndex: 99,
		MarketIndex:       1,
		Status:            perptypes.OrderStatusOpen,
	}
	require.NoError(t, k.SetOrder(ctx, a))
	require.NoError(t, k.IndexClientOrder(ctx, a))
	require.NoError(t, k.IndexAccountOpenOrder(ctx, a))

	// Order B: no client_order_index (default 0)
	b := types.Order{
		OrderIndex:        2,
		ClientOrderIndex:  0,
		OwnerAccountIndex: 99,
		MarketIndex:       1,
		Status:            perptypes.OrderStatusOpen,
	}
	require.NoError(t, k.SetOrder(ctx, b))
	require.NoError(t, k.IndexAccountOpenOrder(ctx, b))

	seen := map[uint64]bool{}
	require.NoError(t, k.IterateAccountOpenOrders(ctx, 99, 0, func(o types.Order) bool {
		seen[o.OrderIndex] = true
		return false
	}))
	require.True(t, seen[1], "order with client_order_index missed")
	require.True(t, seen[2], "order without client_order_index missed")

	// IterateUserOrders by contrast only covers the client-id mapping;
	// it would miss order 2.
	seenLegacy := map[uint64]bool{}
	require.NoError(t, k.IterateUserOrders(ctx, 99, func(o types.Order) bool {
		seenLegacy[o.OrderIndex] = true
		return false
	}))
	require.True(t, seenLegacy[1])
	require.False(t, seenLegacy[2], "legacy iterator unexpectedly covers no-clientID orders")
}

// TestAccountOpenOrders_HonorsMarketFilter checks that a non-zero
// market filter restricts iteration to that market only.
func TestAccountOpenOrders_HonorsMarketFilter(t *testing.T) {
	k, ctx := newOrderbookKeeper(t)

	for i, mkt := range []uint32{1, 2, 1, 3} {
		o := types.Order{
			OrderIndex:        uint64(i + 1),
			OwnerAccountIndex: 7,
			MarketIndex:       mkt,
			Status:            perptypes.OrderStatusOpen,
		}
		require.NoError(t, k.SetOrder(ctx, o))
		require.NoError(t, k.IndexAccountOpenOrder(ctx, o))
	}

	collect := func(filter uint32) []uint64 {
		var got []uint64
		require.NoError(t, k.IterateAccountOpenOrders(ctx, 7, filter, func(o types.Order) bool {
			got = append(got, o.OrderIndex)
			return false
		}))
		return got
	}

	require.ElementsMatch(t, []uint64{1, 2, 3, 4}, collect(0), "filter=0 should yield all markets")
	require.ElementsMatch(t, []uint64{1, 3}, collect(1), "filter=1 should yield market 1 only")
	require.ElementsMatch(t, []uint64{2}, collect(2), "filter=2 should yield market 2 only")
	require.Empty(t, collect(99), "filter for non-existent market should yield nothing")
}

// TestAccountOpenOrders_UnindexRemoves verifies the unindex path drops the
// (account, order_index) tuple from the iterator.
func TestAccountOpenOrders_UnindexRemoves(t *testing.T) {
	k, ctx := newOrderbookKeeper(t)

	o := types.Order{OrderIndex: 5, OwnerAccountIndex: 1, MarketIndex: 1, Status: perptypes.OrderStatusOpen}
	require.NoError(t, k.SetOrder(ctx, o))
	require.NoError(t, k.IndexAccountOpenOrder(ctx, o))

	var hits int
	require.NoError(t, k.IterateAccountOpenOrders(ctx, 1, 0, func(types.Order) bool {
		hits++
		return false
	}))
	require.Equal(t, 1, hits)

	require.NoError(t, k.UnindexAccountOpenOrder(ctx, o))

	hits = 0
	require.NoError(t, k.IterateAccountOpenOrders(ctx, 1, 0, func(types.Order) bool {
		hits++
		return false
	}))
	require.Zero(t, hits)
}
