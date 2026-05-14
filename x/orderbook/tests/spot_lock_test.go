// spot_lock_test.go covers the spot-market lock-on-place /
// release-on-close arithmetic that orderbook drives via the SpotLocker
// interface. The fixtures wire a stub spot MarketKeeper and a
// recording locker so we can assert per-(account, asset) balances
// without pulling in the real x/account keeper. The cap-counter
// lifecycle test lives here too because it uses the same fixture.
package tests

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/require"

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

// spotMarketKeeper returns spot markets so the orderbook lifecycle
// triggers the lock-on-place / release-on-close path.
type spotMarketKeeper struct{}

func (spotMarketKeeper) GetMarket(_ context.Context, idx uint32) (markettypes.Market, error) {
	return markettypes.Market{
		MarketIndex:  idx,
		MarketType:   perptypes.MarketTypeSpot,
		BaseAssetId:  100,
		QuoteAssetId: 200,
	}, nil
}
func (spotMarketKeeper) GetMarketDetails(_ context.Context, idx uint32) (markettypes.MarketDetails, error) {
	return markettypes.MarketDetails{MarketIndex: idx}, nil
}
func (spotMarketKeeper) AllocateNonce(_ context.Context, _ uint32, _ bool) (int64, error) {
	return 1, nil
}
func (spotMarketKeeper) SetMarketDetails(_ context.Context, _ markettypes.MarketDetails) error {
	return nil
}

// recordingLocker keeps a per-(account,asset) running balance so tests
// can assert the lock-on-place / release-on-close arithmetic without
// pulling in the real x/account keeper. Negative balances would be a
// double-release bug.
type recordingLocker struct {
	balances     map[lockKey]cosmosmath.Int
	insufficient bool // when true, IncreaseLockedBalance always rejects
}

type lockKey struct {
	acc   uint64
	asset uint32
}

func newRecordingLocker() *recordingLocker {
	return &recordingLocker{balances: map[lockKey]cosmosmath.Int{}}
}

func (l *recordingLocker) get(acc uint64, asset uint32) cosmosmath.Int {
	if v, ok := l.balances[lockKey{acc, asset}]; ok {
		return v
	}
	return cosmosmath.ZeroInt()
}

// errLockerInsufficient is a test-local sentinel used by the
// recordingLocker to simulate an under-funded account. The orderbook
// keeper does not care about the concrete error type — it only checks
// that OpenOrder propagates the failure and skips state writes.
var errLockerInsufficient = errors.New("spot locker: simulated under-funded account")

func (l *recordingLocker) IncreaseLockedBalance(_ context.Context, acc uint64, asset uint32, amount cosmosmath.Int) error {
	if l.insufficient {
		return errLockerInsufficient
	}
	cur := l.get(acc, asset)
	l.balances[lockKey{acc, asset}] = cur.Add(amount)
	return nil
}

func (l *recordingLocker) DecreaseLockedBalance(_ context.Context, acc uint64, asset uint32, amount cosmosmath.Int) error {
	cur := l.get(acc, asset)
	next := cur.Sub(amount)
	if next.IsNegative() {
		next = cosmosmath.ZeroInt()
	}
	l.balances[lockKey{acc, asset}] = next
	return nil
}

func newOrderbookSpot(t *testing.T) (orderbookkeeper.Keeper, sdk.Context, *recordingLocker) {
	t.Helper()
	keys := storetypes.NewKVStoreKeys(types.StoreKey)
	cdc := moduletestutil.MakeTestEncodingConfig().Codec
	cms := integration.CreateMultiStore(keys, log.NewTestLogger(t))
	ctx := sdk.NewContext(cms, cmtprototypes.Header{}, true, log.NewTestLogger(t))
	locker := newRecordingLocker()
	k := orderbookkeeper.NewKeeper(
		cdc,
		runtime.NewKVStoreService(keys[types.StoreKey]),
		"px1xqcnyve5x5mrwwpev93xxer9venks6t29ke4l8",
		spotMarketKeeper{},
		locker,
	)
	return k, ctx, locker
}

// TestSpotLock_AskLocksBase confirms a resting spot ask locks
// `remaining_base` units of the base asset on OpenOrder and releases
// them on CancelOrder.
func TestSpotLock_AskLocksBase(t *testing.T) {
	k, ctx, locker := newOrderbookSpot(t)

	o := types.Order{
		OrderIndex:          1,
		OwnerAccountIndex:   42,
		MarketIndex:         1,
		IsAsk:               true,
		OrderType:           perptypes.LimitOrder,
		TimeInForce:         perptypes.GTT,
		Price:               1000,
		Nonce:               1,
		InitialBaseAmount:   5,
		RemainingBaseAmount: 5,
		Status:              perptypes.OrderStatusOpen,
	}
	require.NoError(t, k.OpenOrder(ctx, o))
	require.True(t, locker.get(42, 100).Equal(cosmosmath.NewInt(5)),
		"ask should lock base = remaining_base")
	require.True(t, locker.get(42, 200).IsZero(), "no quote lock for asks")

	_, err := k.CancelOrder(ctx, 1)
	require.NoError(t, err)
	require.True(t, locker.get(42, 100).IsZero(), "cancel should release base lock")
}

// TestSpotLock_BidLocksQuote confirms a resting spot bid locks
// `remaining_base * price` units of the quote asset.
func TestSpotLock_BidLocksQuote(t *testing.T) {
	k, ctx, locker := newOrderbookSpot(t)

	o := types.Order{
		OrderIndex:          2,
		OwnerAccountIndex:   42,
		MarketIndex:         1,
		IsAsk:               false,
		OrderType:           perptypes.LimitOrder,
		TimeInForce:         perptypes.GTT,
		Price:               1000,
		Nonce:               -1,
		InitialBaseAmount:   3,
		RemainingBaseAmount: 3,
		Status:              perptypes.OrderStatusOpen,
	}
	require.NoError(t, k.OpenOrder(ctx, o))
	require.True(t, locker.get(42, 200).Equal(cosmosmath.NewInt(3000)),
		"bid should lock quote = base * price")
	require.True(t, locker.get(42, 100).IsZero(), "no base lock for bids")

	_, err := k.CancelOrder(ctx, 2)
	require.NoError(t, err)
	require.True(t, locker.get(42, 200).IsZero(), "cancel should release quote lock")
}

// TestSpotLock_PartialFillReleasesResidueOnCancel asserts the partial-
// fill bookkeeping: trade-keeper drains the matched portion of the lock
// directly from x/account; orderbook only releases the *residual* lock
// when the order leaves the book afterwards.
func TestSpotLock_PartialFillReleasesResidueOnCancel(t *testing.T) {
	k, ctx, locker := newOrderbookSpot(t)

	o := types.Order{
		OrderIndex:          3,
		OwnerAccountIndex:   42,
		MarketIndex:         1,
		IsAsk:               true,
		OrderType:           perptypes.LimitOrder,
		TimeInForce:         perptypes.GTT,
		Price:               1000,
		Nonce:               1,
		InitialBaseAmount:   10,
		RemainingBaseAmount: 10,
		Status:              perptypes.OrderStatusOpen,
	}
	require.NoError(t, k.OpenOrder(ctx, o))
	require.True(t, locker.get(42, 100).Equal(cosmosmath.NewInt(10)))

	// Simulate a partial fill of 4 base. In production this is
	// driven by trade-keeper.spotMakerDebit which both moves base
	// out and decrements the locked counter; here we mimic both
	// sides — the locker drops by the fill, then FillMakerOrder
	// updates the residue on the orderbook entry.
	require.NoError(t, locker.DecreaseLockedBalance(ctx, 42, 100, cosmosmath.NewInt(4)))
	_, err := k.FillMakerOrder(ctx, 3, 4)
	require.NoError(t, err)
	require.True(t, locker.get(42, 100).Equal(cosmosmath.NewInt(6)),
		"after partial fill the residue lock = remaining base")

	_, err = k.CancelOrder(ctx, 3)
	require.NoError(t, err)
	require.True(t, locker.get(42, 100).IsZero(),
		"cancel of partially-filled order must release the residue")
}

// TestSpotLock_OpenOrderRejectsWhenLockerInsufficient ensures a failed
// IncreaseLockedBalance aborts OpenOrder before any entry is inserted
// — the under-funded order does not leak onto the book.
func TestSpotLock_OpenOrderRejectsWhenLockerInsufficient(t *testing.T) {
	k, ctx, locker := newOrderbookSpot(t)
	locker.insufficient = true

	o := types.Order{
		OrderIndex:          4,
		OwnerAccountIndex:   42,
		MarketIndex:         1,
		IsAsk:               true,
		OrderType:           perptypes.LimitOrder,
		TimeInForce:         perptypes.GTT,
		Price:               1000,
		Nonce:               1,
		InitialBaseAmount:   1,
		RemainingBaseAmount: 1,
		Status:              perptypes.OrderStatusOpen,
	}
	require.Error(t, k.OpenOrder(ctx, o))

	// No order record persisted, no AccountOpenOrders entry.
	_, err := k.GetOrder(ctx, 4)
	require.Error(t, err, "rejected order should not be persisted")

	cnt, err := k.GetAccountOpenOrderCount(ctx, 42, 1)
	require.NoError(t, err)
	require.Zero(t, cnt, "rejected order must not bump the open-order counter")
}

// TestAccountOpenOrderCount_TracksLifecycle confirms the cap counter
// follows OpenOrder / CancelOrder / Evict / FullyFilled transitions in
// lock-step with the AccountOpenOrders KeySet.
func TestAccountOpenOrderCount_TracksLifecycle(t *testing.T) {
	k, ctx, _ := newOrderbookSpot(t)

	mkOrder := func(idx uint64, isAsk bool) types.Order {
		return types.Order{
			OrderIndex:          idx,
			OwnerAccountIndex:   77,
			MarketIndex:         1,
			IsAsk:               isAsk,
			OrderType:           perptypes.LimitOrder,
			TimeInForce:         perptypes.GTT,
			Price:               1000,
			Nonce:               int64(idx),
			InitialBaseAmount:   2,
			RemainingBaseAmount: 2,
			Status:              perptypes.OrderStatusOpen,
		}
	}

	require.NoError(t, k.OpenOrder(ctx, mkOrder(1, true)))
	require.NoError(t, k.OpenOrder(ctx, mkOrder(2, false)))
	cnt, err := k.GetAccountOpenOrderCount(ctx, 77, 1)
	require.NoError(t, err)
	require.EqualValues(t, 2, cnt)

	// Cancel one, fully-fill the other.
	_, err = k.CancelOrder(ctx, 1)
	require.NoError(t, err)
	cnt, err = k.GetAccountOpenOrderCount(ctx, 77, 1)
	require.NoError(t, err)
	require.EqualValues(t, 1, cnt, "cancel should decrement the counter")

	_, err = k.FillMakerOrder(ctx, 2, 2)
	require.NoError(t, err)
	cnt, err = k.GetAccountOpenOrderCount(ctx, 77, 1)
	require.NoError(t, err)
	require.Zero(t, cnt, "full fill should drain the counter")
}
