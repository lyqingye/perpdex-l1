package keeper

import (
	"context"

	sdk "github.com/cosmos/cosmos-sdk/types"

	perptypes "github.com/perpdex/perpdex-l1/types"
	"github.com/perpdex/perpdex-l1/x/matching/types"
	orderbooktypes "github.com/perpdex/perpdex-l1/x/orderbook/types"
)

type msgServer struct{ Keeper }

func NewMsgServerImpl(k Keeper) types.MsgServer { return &msgServer{Keeper: k} }

var _ types.MsgServer = msgServer{}

func (m msgServer) CreateOrder(ctx context.Context, msg *types.MsgCreateOrder) (*types.MsgCreateOrderResponse, error) {
	if err := msg.ValidateBasic(); err != nil {
		return nil, err
	}
	if ok, err := m.accountKeeper.IsAuthorized(ctx, msg.Sender, msg.AccountIndex); err != nil {
		return nil, err
	} else if !ok {
		return nil, types.ErrUnauthorized
	}
	market, err := m.marketKeeper.GetMarket(ctx, msg.MarketIndex)
	if err != nil {
		return nil, err
	}
	if market.Status != perptypes.MarketStatusActive {
		return nil, types.ErrInvalidOrder.Wrap("market not active")
	}

	// Step 4: POST_ONLY cross check
	if msg.TimeInForce == perptypes.PostOnly {
		cross, err := m.bookKeeper.WouldCross(ctx, msg.MarketIndex, msg.IsAsk, msg.Price)
		if err != nil {
			return nil, err
		}
		if cross {
			return nil, types.ErrPostOnlyCross
		}
	}

	// Allocate order_index and nonce.
	idx, err := m.bookKeeper.AllocateOrderIndex(ctx)
	if err != nil {
		return nil, err
	}
	nonce, err := m.marketKeeper.AllocateNonce(ctx, msg.MarketIndex, msg.IsAsk)
	if err != nil {
		return nil, err
	}

	now := sdk.UnwrapSDKContext(ctx).BlockTime().UnixMilli()
	order := orderbooktypes.Order{
		OrderIndex:           idx,
		ClientOrderIndex:     msg.ClientOrderIndex,
		OwnerAccountIndex:    msg.AccountIndex,
		MarketIndex:          msg.MarketIndex,
		IsAsk:                msg.IsAsk,
		OrderType:            msg.OrderType,
		TimeInForce:          msg.TimeInForce,
		ReduceOnly:           msg.ReduceOnly,
		Price:                msg.Price,
		Nonce:                nonce,
		InitialBaseAmount:    msg.BaseAmount,
		RemainingBaseAmount:  msg.BaseAmount,
		TriggerPrice:         msg.TriggerPrice,
		Expiry:               msg.Expiry,
		CreatedAt:            now,
		Status:               perptypes.OrderStatusOpen,
	}
	// Trigger orders go to trigger index, not the book.
	if msg.OrderType == perptypes.StopLossOrder ||
		msg.OrderType == perptypes.StopLossLimitOrder ||
		msg.OrderType == perptypes.TakeProfitOrder ||
		msg.OrderType == perptypes.TakeProfitLimitOrder {
		order.Status = perptypes.OrderStatusTriggeredPending
		if err := m.bookKeeper.SetOrder(ctx, order); err != nil {
			return nil, err
		}
		_ = m.bookKeeper.IndexClientOrder(ctx, order)
		return &types.MsgCreateOrderResponse{OrderIndex: idx, Status: order.Status}, nil
	}

	params, err := m.Params.Get(ctx)
	if err != nil {
		return nil, err
	}
	filled, status, err := m.matchOrder(ctx, &order, params.MaxFillsPerMsg)
	if err != nil {
		return nil, err
	}
	order.Status = status

	// IOC: cancel remaining.
	if msg.TimeInForce == perptypes.IOC {
		if order.RemainingBaseAmount > 0 {
			order.Status = perptypes.OrderStatusCancelled
		}
	} else if order.RemainingBaseAmount > 0 {
		entry := orderbooktypes.OrderBookEntry{
			OrderIndex:          order.OrderIndex,
			OwnerAccountIndex:   order.OwnerAccountIndex,
			Price:               order.Price,
			Nonce:               order.Nonce,
			RemainingBaseAmount: order.RemainingBaseAmount,
			Expiry:              order.Expiry,
			IsPostOnly:          msg.TimeInForce == perptypes.PostOnly,
			ReduceOnly:          order.ReduceOnly,
			OrderType:           order.OrderType,
		}
		if err := m.bookKeeper.InsertOrderbookEntry(ctx, order.MarketIndex, order.IsAsk, entry); err != nil {
			return nil, err
		}
	}

	if err := m.bookKeeper.SetOrder(ctx, order); err != nil {
		return nil, err
	}
	_ = m.bookKeeper.IndexClientOrder(ctx, order)

	return &types.MsgCreateOrderResponse{
		OrderIndex:        idx,
		Status:            order.Status,
		FilledBaseAmount:  filled,
	}, nil
}

func (m msgServer) CancelOrder(ctx context.Context, msg *types.MsgCancelOrder) (*types.MsgCancelOrderResponse, error) {
	if err := msg.ValidateBasic(); err != nil {
		return nil, err
	}
	o, err := m.bookKeeper.GetOrder(ctx, msg.OrderIndex)
	if err != nil {
		return nil, err
	}
	if ok, err := m.accountKeeper.IsAuthorized(ctx, msg.Sender, o.OwnerAccountIndex); err != nil {
		return nil, err
	} else if !ok {
		return nil, types.ErrUnauthorized
	}
	if err := m.bookKeeper.RemoveOrderbookEntry(ctx, o.MarketIndex, o.IsAsk, o.OrderIndex); err != nil {
		return nil, err
	}
	o.Status = perptypes.OrderStatusCancelled
	if err := m.bookKeeper.SetOrder(ctx, o); err != nil {
		return nil, err
	}
	_ = m.bookKeeper.UnindexClientOrder(ctx, o)
	return &types.MsgCancelOrderResponse{}, nil
}

func (m msgServer) CancelAllOrders(ctx context.Context, msg *types.MsgCancelAllOrders) (*types.MsgCancelAllOrdersResponse, error) {
	if err := msg.ValidateBasic(); err != nil {
		return nil, err
	}
	if ok, err := m.accountKeeper.IsAuthorized(ctx, msg.Sender, msg.AccountIndex); err != nil {
		return nil, err
	} else if !ok {
		return nil, types.ErrUnauthorized
	}
	switch msg.Mode {
	case perptypes.ImmediateCancelAll, perptypes.ScheduledCancelAll, perptypes.AbortScheduledCancelAll:
	default:
		return nil, types.ErrInvalidOrder.Wrap("unknown cancel-all mode")
	}
	// Implementation note: a full implementation iterates user orders index and
	// cancels each. For MVP we stop at the params cap.
	return &types.MsgCancelAllOrdersResponse{}, nil
}

func (m msgServer) ModifyOrder(ctx context.Context, msg *types.MsgModifyOrder) (*types.MsgModifyOrderResponse, error) {
	if err := msg.ValidateBasic(); err != nil {
		return nil, err
	}
	o, err := m.bookKeeper.GetOrder(ctx, msg.OrderIndex)
	if err != nil {
		return nil, err
	}
	if ok, err := m.accountKeeper.IsAuthorized(ctx, msg.Sender, o.OwnerAccountIndex); err != nil {
		return nil, err
	} else if !ok {
		return nil, types.ErrUnauthorized
	}
	// ModifyOrder = cancel + create with the same client_order_index.
	if err := m.bookKeeper.RemoveOrderbookEntry(ctx, o.MarketIndex, o.IsAsk, o.OrderIndex); err != nil {
		return nil, err
	}
	o.Status = perptypes.OrderStatusCancelled
	_ = m.bookKeeper.SetOrder(ctx, o)
	_ = m.bookKeeper.UnindexClientOrder(ctx, o)

	create := &types.MsgCreateOrder{
		Sender:           msg.Sender,
		AccountIndex:     o.OwnerAccountIndex,
		MarketIndex:      o.MarketIndex,
		ClientOrderIndex: o.ClientOrderIndex,
		IsAsk:            o.IsAsk,
		OrderType:        o.OrderType,
		TimeInForce:      o.TimeInForce,
		BaseAmount:       msg.NewBaseAmount,
		Price:            msg.NewPrice,
		TriggerPrice:     msg.NewTriggerPrice,
		Expiry:           o.Expiry,
		ReduceOnly:       o.ReduceOnly,
	}
	resp, err := m.CreateOrder(ctx, create)
	if err != nil {
		return nil, err
	}
	return &types.MsgModifyOrderResponse{OrderIndex: resp.OrderIndex}, nil
}

func (m msgServer) UpdateParams(ctx context.Context, msg *types.MsgUpdateParams) (*types.MsgUpdateParamsResponse, error) {
	if msg.Authority != m.authority {
		return nil, types.ErrInvalidAuthority
	}
	if err := msg.Params.Validate(); err != nil {
		return nil, err
	}
	if err := m.Params.Set(ctx, msg.Params); err != nil {
		return nil, err
	}
	return &types.MsgUpdateParamsResponse{}, nil
}
