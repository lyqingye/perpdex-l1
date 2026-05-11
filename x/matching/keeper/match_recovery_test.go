package keeper

import (
	"context"
	"fmt"
	"testing"

	"github.com/stretchr/testify/require"

	sdkerrors "cosmossdk.io/errors"

	perptypes "github.com/perpdex/perpdex-l1/types"
	matchingtypes "github.com/perpdex/perpdex-l1/x/matching/types"
	orderbooktypes "github.com/perpdex/perpdex-l1/x/orderbook/types"
	tradekeeper "github.com/perpdex/perpdex-l1/x/trade/keeper"
	tradetypes "github.com/perpdex/perpdex-l1/x/trade/types"
)

// injectingTrade is a TradeKeeper double that returns the next preset
// error from `errs` (consuming one per ApplyPerpsMatching call) so the
// matching loop can be exercised with maker / taker / hard failure
// patterns without standing up the real risk + funding stack.
type injectingTrade struct {
	*stubTrade
	errs []error
}

func (s *injectingTrade) next() error {
	if len(s.errs) == 0 {
		return nil
	}
	err := s.errs[0]
	s.errs = s.errs[1:]
	return err
}

func (s *injectingTrade) ApplyPerpsMatching(ctx context.Context, f tradekeeper.PerpFill) error {
	if err := s.next(); err != nil {
		return err
	}
	return s.stubTrade.ApplyPerpsMatching(ctx, f)
}

func (s *injectingTrade) ApplySpotMatching(ctx context.Context, f tradekeeper.SpotFill, b, q uint32) error {
	if err := s.next(); err != nil {
		return err
	}
	return s.stubTrade.ApplySpotMatching(ctx, f, b, q)
}

// withInjectingTrade returns an env whose tradeKeeper consumes a script
// of injected errors, then falls back to the regular stubTrade behaviour
// for any remaining fills.
func withInjectingTrade(t *testing.T, errs ...error) (*matchEnv, *injectingTrade) {
	t.Helper()
	e := newMatchEnv(t)
	inj := &injectingTrade{stubTrade: e.tk, errs: errs}
	// Rebuild matching keeper bound to the injecting trade.
	e.k.tradeKeeper = inj
	return e, inj
}

// makeMaker / makeTaker are tiny helpers to keep test bodies readable.
func makeMaker(idx, owner uint64, price uint32, base uint64, isAsk bool, nonce int64) orderbooktypes.Order {
	return orderbooktypes.Order{
		OrderIndex:          idx,
		OwnerAccountIndex:   owner,
		MarketIndex:         1,
		IsAsk:               isAsk,
		OrderType:           perptypes.LimitOrder,
		TimeInForce:         perptypes.GTT,
		Price:               price,
		Nonce:               nonce,
		InitialBaseAmount:   base,
		RemainingBaseAmount: base,
		Status:              perptypes.OrderStatusOpen,
	}
}

func makeTaker(idx, owner uint64, price uint32, base uint64, isAsk bool) *orderbooktypes.Order {
	return &orderbooktypes.Order{
		OrderIndex:          idx,
		OwnerAccountIndex:   owner,
		MarketIndex:         1,
		IsAsk:               isAsk,
		OrderType:           perptypes.LimitOrder,
		TimeInForce:         perptypes.GTT,
		Price:               price,
		Nonce:               int64(idx),
		InitialBaseAmount:   base,
		RemainingBaseAmount: base,
		Status:              perptypes.OrderStatusOpen,
	}
}

// TestMatchOrder_BadMakerEvictedAndContinues verifies the
// `cancel_maker_order` semantics: when a single maker fails the
// post-trade risk check, the maker is evicted on the outer ctx and
// the taker continues onto the next price level rather than reverting
// the entire CreateOrder.
func TestMatchOrder_BadMakerEvictedAndContinues(t *testing.T) {
	e, _ := withInjectingTrade(t,
		sdkerrors.Wrapf(tradetypes.ErrMakerRiskRegression, "first maker"),
	)

	// Two asks at the same price; nonce ordering means maker A (lower
	// nonce) is the head of the queue.
	makerA := makeMaker(1, 10, 1000, 5, true, 1)
	makerB := makeMaker(2, 11, 1000, 5, true, 2)
	e.rest(t, makerA, true)
	e.rest(t, makerB, true)

	taker := makeTaker(3, 20, 1000, 5, false)
	filled, status, err := e.k.matchOrder(e.ctx, taker, 16)
	require.NoError(t, err, "soft maker error must not propagate")
	require.EqualValues(t, 5, filled)
	require.Equal(t, perptypes.OrderStatusFilled, status)

	// Bad maker A is evicted (Cancelled), maker B is fully filled.
	a, err := e.bk.GetOrder(e.ctx, 1)
	require.NoError(t, err)
	require.Equal(t, perptypes.OrderStatusCancelled, a.Status, "bad maker should be evicted")

	b, err := e.bk.GetOrder(e.ctx, 2)
	require.NoError(t, err)
	require.Equal(t, perptypes.OrderStatusFilled, b.Status, "next maker should fully fill")
}

// TestMatchOrder_MultipleBadMakersAllEvicted confirms the loop tolerates
// several consecutive bad makers before finding a good one.
func TestMatchOrder_MultipleBadMakersAllEvicted(t *testing.T) {
	e, _ := withInjectingTrade(t,
		sdkerrors.Wrap(tradetypes.ErrMakerRiskRegression, "m1"),
		sdkerrors.Wrap(tradetypes.ErrMakerInsufficientBalance, "m2"),
	)

	m1 := makeMaker(1, 10, 1000, 5, true, 1)
	m2 := makeMaker(2, 11, 1000, 5, true, 2)
	good := makeMaker(3, 12, 1000, 5, true, 3)
	e.rest(t, m1, true)
	e.rest(t, m2, true)
	e.rest(t, good, true)

	taker := makeTaker(4, 20, 1000, 5, false)
	filled, status, err := e.k.matchOrder(e.ctx, taker, 16)
	require.NoError(t, err)
	require.EqualValues(t, 5, filled)
	require.Equal(t, perptypes.OrderStatusFilled, status)

	for _, idx := range []uint64{1, 2} {
		o, err := e.bk.GetOrder(e.ctx, idx)
		require.NoError(t, err)
		require.Equal(t, perptypes.OrderStatusCancelled, o.Status,
			"order %d should have been evicted", idx)
	}
	g, err := e.bk.GetOrder(e.ctx, 3)
	require.NoError(t, err)
	require.Equal(t, perptypes.OrderStatusFilled, g.Status)
}

// TestMatchOrder_BadTakerStopsButPreservesPriorFills confirms the
// `cancel_taker_order` semantics: when the taker fails risk on the
// second iteration, the first fill survives but the residue is
// terminated rather than resting on the book.
func TestMatchOrder_BadTakerStopsButPreservesPriorFills(t *testing.T) {
	e, _ := withInjectingTrade(t,
		nil, // first fill succeeds
		sdkerrors.Wrap(tradetypes.ErrTakerRiskRegression, "taker now broke"),
	)

	m1 := makeMaker(1, 10, 1000, 5, true, 1)
	m2 := makeMaker(2, 11, 1000, 5, true, 2)
	e.rest(t, m1, true)
	e.rest(t, m2, true)

	taker := makeTaker(3, 20, 1000, 10, false)
	filled, status, err := e.k.matchOrder(e.ctx, taker, 16)
	require.NoError(t, err)
	require.EqualValues(t, 5, filled, "first fill should survive")
	require.Equal(t, perptypes.OrderStatusCancelled, status,
		"taker residue is force-cancelled on a recoverable taker error")

	// Maker 1 was filled, maker 2 still rests.
	m1Now, err := e.bk.GetOrder(e.ctx, 1)
	require.NoError(t, err)
	require.Equal(t, perptypes.OrderStatusFilled, m1Now.Status)

	m2Now, err := e.bk.GetOrder(e.ctx, 2)
	require.NoError(t, err)
	require.Equal(t, perptypes.OrderStatusOpen, m2Now.Status,
		"unrelated maker must be untouched")
}

// TestMatchOrder_HardErrorRevertsWholeMatch confirms that a non-sentinel
// trade error short-circuits the matching loop and surfaces to the
// caller, so the cosmos Msg machinery can revert the whole
// transaction. Only the maker that participated in the failing fill is
// affected; previously evicted bad makers from earlier iterations stay
// evicted because they were written through the outer ctx.
func TestMatchOrder_HardErrorRevertsWholeMatch(t *testing.T) {
	hard := fmt.Errorf("simulated funding settle failure")
	e, _ := withInjectingTrade(t, hard)

	m1 := makeMaker(1, 10, 1000, 5, true, 1)
	e.rest(t, m1, true)

	taker := makeTaker(2, 20, 1000, 5, false)
	filled, status, err := e.k.matchOrder(e.ctx, taker, 16)
	require.Error(t, err, "hard error must propagate")
	require.Zero(t, filled)
	require.Equal(t, perptypes.OrderStatusCancelled, status)
}

// TestCreateOrder_PerpOpenOrderCap rejects placement once the account
// has reached Market.MaxOpenOrdersPerAccount.
// `increment_order_count_in_place` aborts when the per-account counter
// is at the limit.
func TestCreateOrder_PerpOpenOrderCap(t *testing.T) {
	e, _ := withInjectingTrade(t)
	e.mk.maxOpenOrders = 2

	srv := NewMsgServerImpl(e.k)
	const (
		account = uint64(42)
		market  = uint32(1)
		sender  = "px1qv9pzxqlyckngw6zf9g9whn9d3eh4qvgsxc8cx"
	)
	build := func(client uint64) *matchingtypes.MsgCreateOrder {
		return &matchingtypes.MsgCreateOrder{
			Sender:           sender,
			AccountIndex:     account,
			MarketIndex:      market,
			ClientOrderIndex: client,
			IsAsk:            true,
			OrderType:        perptypes.LimitOrder,
			TimeInForce:      perptypes.GTT,
			Price:            1000,
			BaseAmount:       1,
		}
	}

	_, err := srv.CreateOrder(e.ctx, build(1))
	require.NoError(t, err, "first within cap")
	_, err = srv.CreateOrder(e.ctx, build(2))
	require.NoError(t, err, "second within cap")
	_, err = srv.CreateOrder(e.ctx, build(3))
	require.Error(t, err, "third must exceed cap")
	require.Contains(t, err.Error(), "open order cap")

	cnt, err := e.bk.GetAccountOpenOrderCount(e.ctx, account, market)
	require.NoError(t, err)
	require.EqualValues(t, 2, cnt)
}

// TestCreateOrder_IOCBypassesCap confirms that an IOC order does not
// count against the cap even when the account is at the limit, because
// IOC residue is force-cancelled and never rests on the book. The
// matching itself is a no-op here (book empty) so the IOC simply
// terminates with no slot consumed.
func TestCreateOrder_IOCBypassesCap(t *testing.T) {
	e, _ := withInjectingTrade(t)
	e.mk.maxOpenOrders = 1

	srv := NewMsgServerImpl(e.k)
	const (
		account = uint64(42)
		market  = uint32(1)
		sender  = "px1qv9pzxqlyckngw6zf9g9whn9d3eh4qvgsxc8cx"
	)
	_, err := srv.CreateOrder(e.ctx, &matchingtypes.MsgCreateOrder{
		Sender: sender, AccountIndex: account, MarketIndex: market,
		ClientOrderIndex: 1, IsAsk: true,
		OrderType: perptypes.LimitOrder, TimeInForce: perptypes.GTT,
		Price: 1000, BaseAmount: 1,
	})
	require.NoError(t, err)

	_, err = srv.CreateOrder(e.ctx, &matchingtypes.MsgCreateOrder{
		Sender: sender, AccountIndex: account, MarketIndex: market,
		ClientOrderIndex: 2, IsAsk: true,
		OrderType: perptypes.LimitOrder, TimeInForce: perptypes.IOC,
		Price: 1000, BaseAmount: 1,
	})
	require.NoError(t, err, "IOC should bypass cap")
}

// TestMatchOrder_BadMakerInvalidPositionEvictedAndContinues confirms
// the `is_new_maker_position_invalid` semantics (case 1 of
// `is_valid_perps_trade`): when a maker's post-trade position would
// overflow the bit-width bound, the maker is evicted on the outer
// ctx and the taker continues.
func TestMatchOrder_BadMakerInvalidPositionEvictedAndContinues(t *testing.T) {
	e, _ := withInjectingTrade(t,
		sdkerrors.Wrap(tradetypes.ErrMakerInvalidPosition, "first maker overflow"),
	)

	makerA := makeMaker(1, 10, 1000, 5, true, 1)
	makerB := makeMaker(2, 11, 1000, 5, true, 2)
	e.rest(t, makerA, true)
	e.rest(t, makerB, true)

	taker := makeTaker(3, 20, 1000, 5, false)
	filled, status, err := e.k.matchOrder(e.ctx, taker, 16)
	require.NoError(t, err, "soft maker error must not propagate")
	require.EqualValues(t, 5, filled)
	require.Equal(t, perptypes.OrderStatusFilled, status)

	a, err := e.bk.GetOrder(e.ctx, 1)
	require.NoError(t, err)
	require.Equal(t, perptypes.OrderStatusCancelled, a.Status,
		"maker with invalid post-trade position must be evicted")

	b, err := e.bk.GetOrder(e.ctx, 2)
	require.NoError(t, err)
	require.Equal(t, perptypes.OrderStatusFilled, b.Status,
		"next maker should fully fill")
}

// TestMatchOrder_BadMakerInsufficientCollateralEvictedAndContinues
// confirms the `is_maker_has_enough_cross_collateral` semantics
// (case 3): a maker whose isolated margin auto-allocation cannot be
// funded from cross collateral is evicted and the loop continues.
func TestMatchOrder_BadMakerInsufficientCollateralEvictedAndContinues(t *testing.T) {
	e, _ := withInjectingTrade(t,
		sdkerrors.Wrap(tradetypes.ErrMakerInsufficientCollateral, "first maker poor"),
	)

	makerA := makeMaker(1, 10, 1000, 5, true, 1)
	makerB := makeMaker(2, 11, 1000, 5, true, 2)
	e.rest(t, makerA, true)
	e.rest(t, makerB, true)

	taker := makeTaker(3, 20, 1000, 5, false)
	filled, status, err := e.k.matchOrder(e.ctx, taker, 16)
	require.NoError(t, err, "soft maker error must not propagate")
	require.EqualValues(t, 5, filled)
	require.Equal(t, perptypes.OrderStatusFilled, status)

	a, err := e.bk.GetOrder(e.ctx, 1)
	require.NoError(t, err)
	require.Equal(t, perptypes.OrderStatusCancelled, a.Status,
		"maker without cross collateral must be evicted")

	b, err := e.bk.GetOrder(e.ctx, 2)
	require.NoError(t, err)
	require.Equal(t, perptypes.OrderStatusFilled, b.Status,
		"next maker should fully fill")
}

// TestMatchOrder_BadTakerInvalidPositionStops confirms the symmetric
// case-1 taker variant: the taker's post-trade position overflowing
// stops further fills but preserves prior fills.
func TestMatchOrder_BadTakerInvalidPositionStops(t *testing.T) {
	e, _ := withInjectingTrade(t,
		nil, // first fill ok
		sdkerrors.Wrap(tradetypes.ErrTakerInvalidPosition, "taker overflow"),
	)

	m1 := makeMaker(1, 10, 1000, 5, true, 1)
	m2 := makeMaker(2, 11, 1000, 5, true, 2)
	e.rest(t, m1, true)
	e.rest(t, m2, true)

	taker := makeTaker(3, 20, 1000, 10, false)
	filled, status, err := e.k.matchOrder(e.ctx, taker, 16)
	require.NoError(t, err)
	require.EqualValues(t, 5, filled, "first fill should survive")
	require.Equal(t, perptypes.OrderStatusCancelled, status,
		"taker residue is force-cancelled on a recoverable taker error")

	m1Now, err := e.bk.GetOrder(e.ctx, 1)
	require.NoError(t, err)
	require.Equal(t, perptypes.OrderStatusFilled, m1Now.Status)

	m2Now, err := e.bk.GetOrder(e.ctx, 2)
	require.NoError(t, err)
	require.Equal(t, perptypes.OrderStatusOpen, m2Now.Status,
		"unrelated maker must be untouched")
}

// TestMatchOrder_BadTakerInsufficientCollateralStops mirrors the
// previous test for the case-3 taker variant.
func TestMatchOrder_BadTakerInsufficientCollateralStops(t *testing.T) {
	e, _ := withInjectingTrade(t,
		nil,
		sdkerrors.Wrap(tradetypes.ErrTakerInsufficientCollateral, "taker poor"),
	)

	m1 := makeMaker(1, 10, 1000, 5, true, 1)
	m2 := makeMaker(2, 11, 1000, 5, true, 2)
	e.rest(t, m1, true)
	e.rest(t, m2, true)

	taker := makeTaker(3, 20, 1000, 10, false)
	filled, status, err := e.k.matchOrder(e.ctx, taker, 16)
	require.NoError(t, err)
	require.EqualValues(t, 5, filled, "first fill should survive")
	require.Equal(t, perptypes.OrderStatusCancelled, status)

	m2Now, err := e.bk.GetOrder(e.ctx, 2)
	require.NoError(t, err)
	require.Equal(t, perptypes.OrderStatusOpen, m2Now.Status,
		"unrelated maker must be untouched")
}

// TestMatchOrder_BadMakerCachePreservesUnfailedFills makes sure that the
// fills committed before a bad maker (writeCache calls) are not rolled
// back when a subsequent iteration fails with a soft error: the loop is
// expected to keep going, and the previously-applied fills should remain
// observable on the trade-keeper's recorded fill log.
func TestMatchOrder_BadMakerCachePreservesUnfailedFills(t *testing.T) {
	e, inj := withInjectingTrade(t,
		nil, // fill 1 ok
		sdkerrors.Wrap(tradetypes.ErrMakerRiskRegression, "m2 bad"),
		nil, // fill 2 ok
	)

	m1 := makeMaker(1, 10, 1000, 5, true, 1)
	m2 := makeMaker(2, 11, 1000, 5, true, 2)
	m3 := makeMaker(3, 12, 1000, 5, true, 3)
	e.rest(t, m1, true)
	e.rest(t, m2, true)
	e.rest(t, m3, true)

	taker := makeTaker(4, 20, 1000, 10, false)
	filled, status, err := e.k.matchOrder(e.ctx, taker, 16)
	require.NoError(t, err)
	require.EqualValues(t, 10, filled)
	require.Equal(t, perptypes.OrderStatusFilled, status)
	// stubTrade only records SUCCESSFUL fills (the failing fill never
	// hits the underlying recorder because injectingTrade short-
	// circuits), so we should see exactly two recorded fills.
	require.Len(t, inj.fills, 2)
}
