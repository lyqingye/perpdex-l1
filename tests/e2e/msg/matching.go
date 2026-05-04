package msg

import (
	sdk "github.com/cosmos/cosmos-sdk/types"

	perp "github.com/perpdex/perpdex-l1/app"
	perptypes "github.com/perpdex/perpdex-l1/types"
	matchingkeeper "github.com/perpdex/perpdex-l1/x/matching/keeper"
	matchingtypes "github.com/perpdex/perpdex-l1/x/matching/types"
	"github.com/perpdex/perpdex-l1/tests/e2e/common"
)

// OrderOpts is the catch-all for MsgCreateOrder; the helper layer fills in
// reasonable defaults for the optional bits that most happy-path tests
// don't care about (TimeInForce defaults to GTT, OrderType to LimitOrder).
type OrderOpts struct {
	MarketIndex      uint32
	IsAsk            bool
	Price            uint32
	BaseAmount       uint64
	ClientOrderIndex uint64
	TimeInForce      uint32
	OrderType        uint32
	TriggerPrice     uint32
	Expiry           int64
	ReduceOnly       bool
	// Sender / AccountIndex are only consulted by PlaceLimitOrderRaw.
	// The standard PlaceLimitOrder fills them from the supplied user.
	Sender       string
	AccountIndex uint64
}

// PlaceLimitOrderRaw bypasses the implicit `user.AccountIndex` mapping
// and instead reads `opts.Sender` + `opts.AccountIndex` directly. Used
// to drive negative scenarios where we want the signer's master to
// authorise an order on a *different* sub-account (e.g. a pool).
func PlaceLimitOrderRaw(
	app *perp.PerpDEXApp,
	ctx sdk.Context,
	opts OrderOpts,
) (*matchingtypes.MsgCreateOrderResponse, error) {
	srv := matchingkeeper.NewMsgServerImpl(app.MatchingKeeper)
	if opts.OrderType == 0 {
		opts.OrderType = perptypes.LimitOrder
	}
	if opts.TimeInForce == 0 {
		opts.TimeInForce = perptypes.GTT
	}
	if opts.ClientOrderIndex == 0 {
		opts.ClientOrderIndex = perptypes.MinClientOrderIndex
	}
	// GTT orders need a positive expiry; fall back to a far-future
	// sentinel when the caller doesn't care about expiry behaviour.
	if opts.TimeInForce == perptypes.GTT && opts.Expiry == 0 {
		opts.Expiry = ctx.BlockTime().UnixMilli() + 365*24*3600*1000
	}
	return srv.CreateOrder(ctx, &matchingtypes.MsgCreateOrder{
		Sender:           opts.Sender,
		AccountIndex:     opts.AccountIndex,
		MarketIndex:      opts.MarketIndex,
		ClientOrderIndex: opts.ClientOrderIndex,
		IsAsk:            opts.IsAsk,
		OrderType:        opts.OrderType,
		TimeInForce:      opts.TimeInForce,
		BaseAmount:       opts.BaseAmount,
		Price:            opts.Price,
		TriggerPrice:     opts.TriggerPrice,
		Expiry:           opts.Expiry,
		ReduceOnly:       opts.ReduceOnly,
	})
}

// PlaceLimitOrder is the minimal sugar over MsgCreateOrder for a GTT limit
// order. Returns the assigned OrderIndex and how much of the order was
// filled immediately by the matching engine.
func PlaceLimitOrder(
	app *perp.PerpDEXApp,
	ctx sdk.Context,
	user common.TestUser,
	opts OrderOpts,
) (*matchingtypes.MsgCreateOrderResponse, error) {
	srv := matchingkeeper.NewMsgServerImpl(app.MatchingKeeper)
	if opts.OrderType == 0 {
		opts.OrderType = perptypes.LimitOrder
	}
	if opts.TimeInForce == 0 {
		opts.TimeInForce = perptypes.GTT
	}
	if opts.ClientOrderIndex == 0 {
		opts.ClientOrderIndex = perptypes.MinClientOrderIndex
	}
	// GTT orders now require expiry > 0; provide a far-future default so
	// legacy helpers that only cared about price / size still compile.
	if opts.TimeInForce == perptypes.GTT && opts.Expiry == 0 {
		opts.Expiry = ctx.BlockTime().UnixMilli() + 365*24*3600*1000
	}
	return srv.CreateOrder(ctx, &matchingtypes.MsgCreateOrder{
		Sender:           user.Address.String(),
		AccountIndex:     user.AccountIndex,
		MarketIndex:      opts.MarketIndex,
		ClientOrderIndex: opts.ClientOrderIndex,
		IsAsk:            opts.IsAsk,
		OrderType:        opts.OrderType,
		TimeInForce:      opts.TimeInForce,
		BaseAmount:       opts.BaseAmount,
		Price:            opts.Price,
		TriggerPrice:     opts.TriggerPrice,
		Expiry:           opts.Expiry,
		ReduceOnly:       opts.ReduceOnly,
	})
}

// PlaceMarketOrder forces TimeInForce=IOC + Price=0 so the matching engine
// crosses against any available depth.
func PlaceMarketOrder(
	app *perp.PerpDEXApp,
	ctx sdk.Context,
	user common.TestUser,
	marketIndex uint32,
	isAsk bool,
	baseAmount uint64,
	clientOrderIndex uint64,
) (*matchingtypes.MsgCreateOrderResponse, error) {
	srv := matchingkeeper.NewMsgServerImpl(app.MatchingKeeper)
	if clientOrderIndex == 0 {
		clientOrderIndex = perptypes.MinClientOrderIndex
	}
	// Price boundary for a marketable IOC: ask uses 1, bid uses MAX so that
	// the matching engine never rejects on price-out-of-range.
	price := uint32(perptypes.MaxOrderPrice)
	if isAsk {
		price = 1
	}
	return srv.CreateOrder(ctx, &matchingtypes.MsgCreateOrder{
		Sender:           user.Address.String(),
		AccountIndex:     user.AccountIndex,
		MarketIndex:      marketIndex,
		ClientOrderIndex: clientOrderIndex,
		IsAsk:            isAsk,
		OrderType:        perptypes.MarketOrder,
		TimeInForce:      perptypes.IOC,
		BaseAmount:       baseAmount,
		Price:            price,
	})
}

// CancelOrder removes a single resting order belonging to `user`.
func CancelOrder(
	app *perp.PerpDEXApp,
	ctx sdk.Context,
	user common.TestUser,
	marketIndex uint32,
	orderIndex uint64,
) (*matchingtypes.MsgCancelOrderResponse, error) {
	srv := matchingkeeper.NewMsgServerImpl(app.MatchingKeeper)
	return srv.CancelOrder(ctx, &matchingtypes.MsgCancelOrder{
		Sender:      user.Address.String(),
		MarketIndex: marketIndex,
		OrderIndex:  orderIndex,
	})
}

// ModifyOrder atomically cancels and recreates an order in place; the
// underlying matching keeper preserves the same client_order_index.
func ModifyOrder(
	app *perp.PerpDEXApp,
	ctx sdk.Context,
	user common.TestUser,
	marketIndex, newPrice uint32,
	orderIndex, newBaseAmount uint64,
	newTriggerPrice uint32,
) (*matchingtypes.MsgModifyOrderResponse, error) {
	srv := matchingkeeper.NewMsgServerImpl(app.MatchingKeeper)
	return srv.ModifyOrder(ctx, &matchingtypes.MsgModifyOrder{
		Sender:          user.Address.String(),
		MarketIndex:     marketIndex,
		OrderIndex:      orderIndex,
		NewPrice:        newPrice,
		NewBaseAmount:   newBaseAmount,
		NewTriggerPrice: newTriggerPrice,
	})
}
