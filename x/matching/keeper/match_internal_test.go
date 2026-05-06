package keeper

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"

	cmtprototypes "github.com/cometbft/cometbft/proto/tendermint/types"

	"cosmossdk.io/log"
	"cosmossdk.io/math"
	storetypes "cosmossdk.io/store/types"

	"github.com/cosmos/cosmos-sdk/runtime"
	"github.com/cosmos/cosmos-sdk/testutil/integration"
	sdk "github.com/cosmos/cosmos-sdk/types"
	moduletestutil "github.com/cosmos/cosmos-sdk/types/module/testutil"

	perptypes "github.com/perpdex/perpdex-l1/types"
	accounttypes "github.com/perpdex/perpdex-l1/x/account/types"
	markettypes "github.com/perpdex/perpdex-l1/x/market/types"
	matchingtypes "github.com/perpdex/perpdex-l1/x/matching/types"
	orderbookkeeper "github.com/perpdex/perpdex-l1/x/orderbook/keeper"
	orderbooktypes "github.com/perpdex/perpdex-l1/x/orderbook/types"
	tradekeeper "github.com/perpdex/perpdex-l1/x/trade/keeper"
)

// --- minimal stubs for the matching dependencies --------------------------

type stubAccount struct {
	pos map[[2]uint64]accounttypes.AccountPosition
}

func newStubAccount() *stubAccount {
	return &stubAccount{pos: map[[2]uint64]accounttypes.AccountPosition{}}
}

func key(acc uint64, mkt uint32) [2]uint64 { return [2]uint64{acc, uint64(mkt)} }

func (s *stubAccount) GetAccount(_ context.Context, idx uint64) (accounttypes.Account, error) {
	return accounttypes.Account{AccountIndex: idx, AccountType: perptypes.MasterAccountType, Collateral: math.ZeroInt()}, nil
}

func (s *stubAccount) GetPosition(_ context.Context, acc uint64, mkt uint32) (accounttypes.AccountPosition, error) {
	if p, ok := s.pos[key(acc, mkt)]; ok {
		return p, nil
	}
	return accounttypes.AccountPosition{
		AccountIndex:             acc,
		MarketIndex:              mkt,
		Position:                 math.ZeroInt(),
		EntryQuote:               math.ZeroInt(),
		LastFundingRatePrefixSum: math.ZeroInt(),
		AllocatedMargin:          math.ZeroInt(),
		MarginMode:               perptypes.CrossMargin,
	}, nil
}

func (s *stubAccount) setPosition(acc uint64, mkt uint32, size int64) {
	s.pos[key(acc, mkt)] = accounttypes.AccountPosition{
		AccountIndex:             acc,
		MarketIndex:              mkt,
		Position:                 math.NewInt(size),
		EntryQuote:               math.ZeroInt(),
		LastFundingRatePrefixSum: math.ZeroInt(),
		AllocatedMargin:          math.ZeroInt(),
		MarginMode:               perptypes.CrossMargin,
	}
}

func (s *stubAccount) IsAuthorized(_ context.Context, _ string, _ uint64) (bool, error) {
	return true, nil
}

func (s *stubAccount) AvailableBalance(_ context.Context, _ uint64, _ uint32) (math.Int, error) {
	// Stub assumes infinite spot liquidity for matching tests; the
	// dedicated spot lock tests live in x/orderbook keeper tests.
	return math.NewInt(1 << 62), nil
}

// stubLocker is a no-op SpotLocker used by orderbook tests that do not
// exercise spot lock semantics directly.
type stubLocker struct{}

func (stubLocker) IncreaseLockedBalance(_ context.Context, _ uint64, _ uint32, _ math.Int) error {
	return nil
}
func (stubLocker) DecreaseLockedBalance(_ context.Context, _ uint64, _ uint32, _ math.Int) error {
	return nil
}

type stubMarket struct {
	maxOpenOrders uint32
	nextNonceAsk  int64
	nextNonceBid  int64
}

func (s *stubMarket) GetMarket(_ context.Context, idx uint32) (markettypes.Market, error) {
	return markettypes.Market{
		MarketIndex:             idx,
		MarketType:              perptypes.MarketTypePerps,
		Status:                  perptypes.MarketStatusActive,
		TakerFee:                0,
		MakerFee:                0,
		MaxOpenOrdersPerAccount: s.maxOpenOrders,
	}, nil
}

func (*stubMarket) GetMarketDetails(_ context.Context, idx uint32) (markettypes.MarketDetails, error) {
	return markettypes.MarketDetails{MarketIndex: idx}, nil
}

func (s *stubMarket) AllocateNonce(_ context.Context, _ uint32, isAsk bool) (int64, error) {
	if isAsk {
		s.nextNonceAsk++
		return s.nextNonceAsk, nil
	}
	s.nextNonceBid--
	return s.nextNonceBid, nil
}

func (*stubMarket) SetMarketDetails(_ context.Context, _ markettypes.MarketDetails) error {
	return nil
}

// stubTrade records every fill it sees and applies the position delta to
// the linked stubAccount so subsequent matching iterations see the
// updated maker/taker positions (mirroring real trade-keeper behaviour).
type stubTrade struct {
	ak    *stubAccount
	fills []tradekeeper.Fill
}

func (s *stubTrade) applyDelta(acc uint64, mkt uint32, delta int64) {
	cur, _ := s.ak.GetPosition(context.Background(), acc, mkt)
	cur.Position = cur.Position.Add(math.NewInt(delta))
	s.ak.pos[key(acc, mkt)] = cur
}

func (s *stubTrade) ApplyPerpsMatching(_ context.Context, f tradekeeper.Fill) error {
	s.fills = append(s.fills, f)
	base := int64(f.BaseAmount)
	if f.IsTakerAsk {
		// taker sells, maker buys
		s.applyDelta(f.TakerAccountIndex, f.MarketIndex, -base)
		s.applyDelta(f.MakerAccountIndex, f.MarketIndex, +base)
	} else {
		// taker buys, maker sells
		s.applyDelta(f.TakerAccountIndex, f.MarketIndex, +base)
		s.applyDelta(f.MakerAccountIndex, f.MarketIndex, -base)
	}
	return nil
}

func (s *stubTrade) ApplySpotMatching(_ context.Context, f tradekeeper.Fill, _, _ uint32) error {
	s.fills = append(s.fills, f)
	return nil
}

// --- test fixture ---------------------------------------------------------

type matchEnv struct {
	ctx sdk.Context
	ak  *stubAccount
	tk  *stubTrade
	bk  orderbookkeeper.Keeper
	k   Keeper
	mk  *stubMarket
}

func newMatchEnv(t *testing.T) *matchEnv {
	t.Helper()
	keys := storetypes.NewKVStoreKeys(orderbooktypes.StoreKey, matchingtypes.StoreKey)
	cdc := moduletestutil.MakeTestEncodingConfig().Codec
	cms := integration.CreateMultiStore(keys, log.NewTestLogger(t))
	ctx := sdk.NewContext(cms, cmtprototypes.Header{}, true, log.NewTestLogger(t))

	mk := &stubMarket{}
	bk := orderbookkeeper.NewKeeper(
		cdc,
		runtime.NewKVStoreService(keys[orderbooktypes.StoreKey]),
		"px1xqcnyve5x5mrwwpev93xxer9venks6t29ke4l8",
		mk,
		stubLocker{},
	)

	ak := newStubAccount()
	tk := &stubTrade{ak: ak}
	k := NewKeeper(
		cdc,
		runtime.NewKVStoreService(keys[matchingtypes.StoreKey]),
		"px1xqcnyve5x5mrwwpev93xxer9venks6t29ke4l8",
		ak,
		mk,
		bk,
		tk,
	)
	require.NoError(t, k.Params.Set(ctx, matchingtypes.Params{MaxFillsPerMsg: 64, MaxCancelsPerMsg: 128}))

	return &matchEnv{ctx: ctx, ak: ak, tk: tk, bk: bk, k: k, mk: mk}
}

// rest places a maker order on the book through the public OpenOrder
// lifecycle, mirroring how msgServer.CreateOrder does it. The `isAsk`
// argument is preserved for callsite readability — OpenOrder reads the
// side from o.IsAsk.
func (e *matchEnv) rest(t *testing.T, o orderbooktypes.Order, _ bool) {
	t.Helper()
	require.NoError(t, e.bk.OpenOrder(e.ctx, o, false))
}

// TestMatchOrder_MarketOrderBidAtZeroPrice ensures a buy MarketOrder with
// Price=0 (the canonical no-limit-price form, also produced by activated
// STOP/TAKE triggers) is no longer cancelled at the limit-price gate.
func TestMatchOrder_MarketOrderBidAtZeroPrice(t *testing.T) {
	e := newMatchEnv(t)

	maker := orderbooktypes.Order{
		OrderIndex:          1,
		OwnerAccountIndex:   10,
		MarketIndex:         1,
		IsAsk:               true,
		OrderType:           perptypes.LimitOrder,
		TimeInForce:         perptypes.GTT,
		Price:               1000,
		Nonce:               1,
		InitialBaseAmount:   5,
		RemainingBaseAmount: 5,
		Status:              perptypes.OrderStatusOpen,
		Expiry:              0,
	}
	e.rest(t, maker, true)

	taker := &orderbooktypes.Order{
		OrderIndex:          2,
		OwnerAccountIndex:   20,
		MarketIndex:         1,
		IsAsk:               false,
		OrderType:           perptypes.MarketOrder,
		TimeInForce:         perptypes.IOC,
		Price:               0, // no-limit-price
		Nonce:               2,
		InitialBaseAmount:   5,
		RemainingBaseAmount: 5,
		Status:              perptypes.OrderStatusOpen,
	}

	filled, status, err := e.k.matchOrder(e.ctx, taker, 16)
	require.NoError(t, err)
	require.EqualValues(t, 5, filled)
	require.Equal(t, perptypes.OrderStatusFilled, status)
	require.Len(t, e.tk.fills, 1)
	require.EqualValues(t, 5, e.tk.fills[0].BaseAmount)
}

// TestCancelAllOrders_HonorsMarketFilter ensures that an explicit
// MarketIndexFilter restricts the cancel-all sweep to that market only,
// and that filter==0 sweeps every market (proto contract).
func TestCancelAllOrders_HonorsMarketFilter(t *testing.T) {
	e := newMatchEnv(t)
	srv := NewMsgServerImpl(e.k)

	// Two resting orders, market 1 and market 2, same account, no client id.
	o1 := orderbooktypes.Order{
		OrderIndex: 1, OwnerAccountIndex: 99, MarketIndex: 1, IsAsk: true,
		OrderType: perptypes.LimitOrder, TimeInForce: perptypes.GTT,
		Price: 1000, Nonce: 1, InitialBaseAmount: 5, RemainingBaseAmount: 5,
		Status: perptypes.OrderStatusOpen,
	}
	o2 := orderbooktypes.Order{
		OrderIndex: 2, OwnerAccountIndex: 99, MarketIndex: 2, IsAsk: true,
		OrderType: perptypes.LimitOrder, TimeInForce: perptypes.GTT,
		Price: 2000, Nonce: 1, InitialBaseAmount: 5, RemainingBaseAmount: 5,
		Status: perptypes.OrderStatusOpen,
	}
	e.rest(t, o1, true)
	e.rest(t, o2, true)

	// Cancel only market 1.
	_, err := srv.CancelAllOrders(e.ctx, &matchingtypes.MsgCancelAllOrders{
		Sender:            "px1qv9pzxqlyckngw6zf9g9whn9d3eh4qvgsxc8cx",
		AccountIndex:      99,
		MarketIndexFilter: 1,
		Mode:              perptypes.ImmediateCancelAll,
	})
	require.NoError(t, err)

	o1Now, err := e.bk.GetOrder(e.ctx, 1)
	require.NoError(t, err)
	require.Equal(t, perptypes.OrderStatusCancelled, o1Now.Status)

	o2Now, err := e.bk.GetOrder(e.ctx, 2)
	require.NoError(t, err)
	require.Equal(t, perptypes.OrderStatusOpen, o2Now.Status, "market 2 must be untouched")
}

// TestCancelAllOrders_CoversOrdersWithoutClientID verifies that an order
// whose ClientOrderIndex is 0 (the optional default) is still reachable
// via cancel-all. The legacy IterateUserOrders path missed these.
func TestCancelAllOrders_CoversOrdersWithoutClientID(t *testing.T) {
	e := newMatchEnv(t)
	srv := NewMsgServerImpl(e.k)

	o := orderbooktypes.Order{
		OrderIndex: 7, OwnerAccountIndex: 42, MarketIndex: 1, IsAsk: false,
		OrderType: perptypes.LimitOrder, TimeInForce: perptypes.GTT,
		Price: 100, Nonce: 1, InitialBaseAmount: 1, RemainingBaseAmount: 1,
		ClientOrderIndex: 0, // explicit: not set
		Status:           perptypes.OrderStatusOpen,
	}
	e.rest(t, o, false)

	_, err := srv.CancelAllOrders(e.ctx, &matchingtypes.MsgCancelAllOrders{
		Sender:            "px1qv9pzxqlyckngw6zf9g9whn9d3eh4qvgsxc8cx",
		AccountIndex:      42,
		MarketIndexFilter: 0,
		Mode:              perptypes.ImmediateCancelAll,
	})
	require.NoError(t, err)

	got, err := e.bk.GetOrder(e.ctx, 7)
	require.NoError(t, err)
	require.Equal(t, perptypes.OrderStatusCancelled, got.Status)
}

// TestMatchOrder_EvictReduceOnlyClearsOrderRecord is the regression test
// for the historical leak where matchOrder would only call
// RemoveOrderbookEntry when a reduce-only maker was found to be invalid
// (no opposite-direction position), leaving the maker Order record
// stuck at Status=Open and its client / account-open indexes alive.
//
// After the orderbook lifecycle refactor, this path goes through
// EvictMakerOrder which atomically: removes the entry, marks the Order
// Cancelled, and clears the client + account-open indexes — so a stale
// "open" order can no longer linger after eviction.
func TestMatchOrder_EvictReduceOnlyClearsOrderRecord(t *testing.T) {
	e := newMatchEnv(t)

	// maker account 10 holds no position, so the reduce-only ask is
	// invalid the moment the taker bids against it.
	maker := orderbooktypes.Order{
		OrderIndex:          1,
		ClientOrderIndex:    7,
		OwnerAccountIndex:   10,
		MarketIndex:         1,
		IsAsk:               true,
		OrderType:           perptypes.LimitOrder,
		TimeInForce:         perptypes.GTT,
		Price:               1000,
		Nonce:               1,
		InitialBaseAmount:   5,
		RemainingBaseAmount: 5,
		ReduceOnly:          true,
		Status:              perptypes.OrderStatusOpen,
	}
	e.rest(t, maker, true)

	// Sanity: the AccountOpenOrders index sees the maker as resting.
	var pre int
	require.NoError(t, e.bk.IterateAccountOpenOrders(e.ctx, 10, 0, func(orderbooktypes.Order) bool {
		pre++
		return false
	}))
	require.Equal(t, 1, pre)

	taker := &orderbooktypes.Order{
		OrderIndex:          2,
		OwnerAccountIndex:   20,
		MarketIndex:         1,
		IsAsk:               false,
		OrderType:           perptypes.LimitOrder,
		TimeInForce:         perptypes.GTT,
		Price:               1000,
		Nonce:               2,
		InitialBaseAmount:   5,
		RemainingBaseAmount: 5,
		Status:              perptypes.OrderStatusOpen,
	}

	filled, _, err := e.k.matchOrder(e.ctx, taker, 16)
	require.NoError(t, err)
	require.Zero(t, filled, "reduce-only maker without position must not produce a fill")
	require.Empty(t, e.tk.fills)

	// Maker Order record is now Cancelled (was previously stuck Open).
	got, err := e.bk.GetOrder(e.ctx, 1)
	require.NoError(t, err)
	require.Equal(t, perptypes.OrderStatusCancelled, got.Status)

	// Client + account-open indexes are cleared.
	_, err = e.bk.GetOrderByClientID(e.ctx, 1, 10, 7)
	require.Error(t, err, "client_order_index mapping should be removed after eviction")

	var post int
	require.NoError(t, e.bk.IterateAccountOpenOrders(e.ctx, 10, 0, func(orderbooktypes.Order) bool {
		post++
		return false
	}))
	require.Zero(t, post, "evicted reduce-only maker must not survive in AccountOpenOrders")
}

// TestMatchOrder_MakerReduceOnlyNoFlip enforces that a reduce-only maker
// cannot flip its own position even if the taker requests more base than
// the maker actually holds. With maker long=5 and taker bid 10 against a
// reduce-only ask of size 10, only 5 may fill.
func TestMatchOrder_MakerReduceOnlyNoFlip(t *testing.T) {
	e := newMatchEnv(t)

	// maker is long 5
	e.ak.setPosition(10, 1, 5)

	maker := orderbooktypes.Order{
		OrderIndex:          1,
		OwnerAccountIndex:   10,
		MarketIndex:         1,
		IsAsk:               true,
		OrderType:           perptypes.LimitOrder,
		TimeInForce:         perptypes.GTT,
		Price:               1000,
		Nonce:               1,
		InitialBaseAmount:   10,
		RemainingBaseAmount: 10,
		ReduceOnly:          true,
		Status:              perptypes.OrderStatusOpen,
	}
	e.rest(t, maker, true)

	taker := &orderbooktypes.Order{
		OrderIndex:          2,
		OwnerAccountIndex:   20,
		MarketIndex:         1,
		IsAsk:               false,
		OrderType:           perptypes.LimitOrder,
		TimeInForce:         perptypes.GTT,
		Price:               1000,
		Nonce:               2,
		InitialBaseAmount:   10,
		RemainingBaseAmount: 10,
		Status:              perptypes.OrderStatusOpen,
	}

	filled, _, err := e.k.matchOrder(e.ctx, taker, 16)
	require.NoError(t, err)
	require.EqualValues(t, 5, filled, "maker reduce-only must cap fill to maker's |position|")
	require.Len(t, e.tk.fills, 1)
	require.EqualValues(t, 5, e.tk.fills[0].BaseAmount)
}
