package keeper

import (
	"context"

	"cosmossdk.io/math"
	sdk "github.com/cosmos/cosmos-sdk/types"

	perptypes "github.com/perpdex/perpdex-l1/types"
	"github.com/perpdex/perpdex-l1/x/matching/types"
	orderbooktypes "github.com/perpdex/perpdex-l1/x/orderbook/types"
)

type msgServer struct{ Keeper }

func NewMsgServerImpl(k Keeper) types.MsgServer { return &msgServer{Keeper: k} }

var _ types.MsgServer = msgServer{}

// isTriggerOrder is a local predicate replicating the one in msgs.go so
// keeper code can avoid importing types.isTriggerOrderType.
func isTriggerOrder(t uint32) bool {
	return t == perptypes.StopLossOrder ||
		t == perptypes.StopLossLimitOrder ||
		t == perptypes.TakeProfitOrder ||
		t == perptypes.TakeProfitLimitOrder
}

// quoteExceedsLimit returns true when base * price exceeds the market's
// configured `OrderQuoteLimit`. The multiplication is done over `math.Int`
// (arbitrary-precision) so overflow can never wrap a malicious order back
// under the cap — even with `base ~ 2^48` and `price ~ 2^32` legal inputs.
func quoteExceedsLimit(base uint64, price uint32, limit int64) bool {
	if limit <= 0 || base == 0 || price == 0 {
		return false
	}
	prod := math.NewIntFromUint64(base).Mul(math.NewIntFromUint64(uint64(price)))
	return prod.GT(math.NewInt(limit))
}

func (m msgServer) CreateOrder(ctx context.Context, msg *types.MsgCreateOrder) (*types.MsgCreateOrderResponse, error) {
	if ok, err := m.accountKeeper.IsAuthorized(ctx, msg.Sender, msg.AccountIndex); err != nil {
		return nil, err
	} else if !ok {
		return nil, types.ErrUnauthorized
	}
	// Public pools and the Insurance Fund cannot place orders directly;
	// their fills come exclusively from the liquidation / ADL paths
	// (lighter parity: l2_create_order rejects PUBLIC_POOL_ACCOUNT_TYPE
	// and INSURANCE_FUND_ACCOUNT_TYPE up front).
	if acc, err := m.accountKeeper.GetAccount(ctx, msg.AccountIndex); err == nil {
		if acc.AccountType == perptypes.PublicPoolAccountType ||
			acc.AccountType == perptypes.InsuranceFundAccountType {
			return nil, types.ErrPoolCannotPlaceOrder
		}
	}
	market, err := m.marketKeeper.GetMarket(ctx, msg.MarketIndex)
	if err != nil {
		return nil, err
	}
	if market.Status != perptypes.MarketStatusActive {
		return nil, types.ErrInvalidOrder.Wrap("market not active")
	}
	// Market-configured minima and per-order quote cap.
	if market.MinBaseAmount > 0 && msg.BaseAmount < market.MinBaseAmount {
		return nil, types.ErrInvalidOrder.Wrapf("base_amount %d below market min %d", msg.BaseAmount, market.MinBaseAmount)
	}
	if market.MinQuoteAmount > 0 && msg.Price > 0 {
		if uint64(msg.Price)*msg.BaseAmount < market.MinQuoteAmount {
			return nil, types.ErrInvalidOrder.Wrapf("quote notional below market min %d", market.MinQuoteAmount)
		}
	}
	if quoteExceedsLimit(msg.BaseAmount, msg.Price, market.OrderQuoteLimit) {
		return nil, types.ErrQuoteLimitExceeded
	}

	// Duplicate client_order_index: if an open/pending order already
	// owns the same tuple, reject so a cancel-old / create-new race
	// cannot orphan the index.
	if msg.ClientOrderIndex != 0 {
		has, _, err := m.bookKeeper.HasOpenClientOrder(ctx, msg.MarketIndex, msg.AccountIndex, msg.ClientOrderIndex)
		if err != nil {
			return nil, err
		}
		if has {
			return nil, types.ErrDuplicateClientOrder
		}
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

	// Reduce-only pre-check: taker reduce-only can only be filled when
	// the account currently holds an opposite-side position.
	if msg.ReduceOnly {
		if ok, err := m.reduceOnlyCompatible(ctx, msg.AccountIndex, msg.MarketIndex, msg.IsAsk, msg.BaseAmount); err != nil {
			return nil, err
		} else if !ok {
			return nil, types.ErrReduceOnlyViolated
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
		OrderIndex:          idx,
		ClientOrderIndex:    msg.ClientOrderIndex,
		OwnerAccountIndex:   msg.AccountIndex,
		MarketIndex:         msg.MarketIndex,
		IsAsk:               msg.IsAsk,
		OrderType:           msg.OrderType,
		TimeInForce:         msg.TimeInForce,
		ReduceOnly:          msg.ReduceOnly,
		Price:               msg.Price,
		Nonce:               nonce,
		InitialBaseAmount:   msg.BaseAmount,
		RemainingBaseAmount: msg.BaseAmount,
		TriggerPrice:        msg.TriggerPrice,
		Expiry:              msg.Expiry,
		CreatedAt:           now,
		Status:              perptypes.OrderStatusOpen,
	}
	// Trigger orders (stop/take) are parked in the trigger index until a
	// price crossover activates them in EndBlocker.
	if isTriggerOrder(msg.OrderType) {
		order.Status = perptypes.OrderStatusTriggeredPending
		order.TriggerStatus = perptypes.TriggerStatusMarkPrice
		if err := m.bookKeeper.AddTrigger(ctx, msg.MarketIndex, msg.TriggerPrice, idx); err != nil {
			return nil, err
		}
		if err := m.bookKeeper.SetOrder(ctx, order); err != nil {
			return nil, err
		}
		if err := m.bookKeeper.IndexClientOrder(ctx, order); err != nil {
			return nil, err
		}
		if err := m.bookKeeper.IndexAccountOpenOrder(ctx, order); err != nil {
			return nil, err
		}
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
	// Only index persistent (open/partial) orders. Fully filled / cancelled
	// IOC orders don't need client-id lookup; indexing them would also
	// leave stale mappings around after they leave the book.
	switch order.Status {
	case perptypes.OrderStatusOpen, perptypes.OrderStatusPartiallyFilled:
		if err := m.bookKeeper.IndexClientOrder(ctx, order); err != nil {
			return nil, err
		}
		if err := m.bookKeeper.IndexAccountOpenOrder(ctx, order); err != nil {
			return nil, err
		}
	}

	return &types.MsgCreateOrderResponse{
		OrderIndex:       idx,
		Status:           order.Status,
		FilledBaseAmount: filled,
	}, nil
}

// reduceOnlyCompatible reports whether a reduce-only order on `isAsk` side
// with `baseAmount` size can legitimately only reduce the account's current
// position: the account must be net-positive/negative on the opposite side.
func (m msgServer) reduceOnlyCompatible(ctx context.Context, accIdx uint64, marketIdx uint32, isAsk bool, _ uint64) (bool, error) {
	pos, err := m.accountKeeper.GetPosition(ctx, accIdx, marketIdx)
	if err != nil {
		return false, err
	}
	if pos.Position.IsZero() {
		return false, nil
	}
	// Taker ask (seller) must be long; taker bid (buyer) must be short.
	if isAsk {
		return pos.Position.IsPositive(), nil
	}
	return pos.Position.IsNegative(), nil
}

func (m msgServer) CancelOrder(ctx context.Context, msg *types.MsgCancelOrder) (*types.MsgCancelOrderResponse, error) {
	o, err := m.bookKeeper.GetOrder(ctx, msg.OrderIndex)
	if err != nil {
		return nil, err
	}
	if ok, err := m.accountKeeper.IsAuthorized(ctx, msg.Sender, o.OwnerAccountIndex); err != nil {
		return nil, err
	} else if !ok {
		return nil, types.ErrUnauthorized
	}
	if err := m.cancelOrderInternal(ctx, o); err != nil {
		return nil, err
	}
	return &types.MsgCancelOrderResponse{}, nil
}

// cancelOrderInternal is the shared cancel path used by CancelOrder,
// CancelAllOrders and ModifyOrder. It enforces the order-status state
// machine so history entries (filled / already-cancelled) cannot be
// overwritten, and routes trigger-pending orders through the trigger
// index cleanup.
func (m msgServer) cancelOrderInternal(ctx context.Context, o orderbooktypes.Order) error {
	switch o.Status {
	case perptypes.OrderStatusOpen, perptypes.OrderStatusPartiallyFilled:
		if o.RemainingBaseAmount == 0 {
			return types.ErrOrderNotCancelable.Wrapf("order_index=%d already fully filled", o.OrderIndex)
		}
		if err := m.bookKeeper.RemoveOrderbookEntry(ctx, o.MarketIndex, o.IsAsk, o.OrderIndex); err != nil {
			return err
		}
	case perptypes.OrderStatusTriggeredPending:
		if err := m.bookKeeper.RemoveTrigger(ctx, o.MarketIndex, o.TriggerPrice, o.OrderIndex); err != nil {
			return err
		}
	default:
		return types.ErrOrderNotCancelable.Wrapf("order_index=%d status=%d", o.OrderIndex, o.Status)
	}
	o.Status = perptypes.OrderStatusCancelled
	if err := m.bookKeeper.SetOrder(ctx, o); err != nil {
		return err
	}
	if err := m.bookKeeper.UnindexClientOrderIfMatches(ctx, o); err != nil {
		return err
	}
	return m.bookKeeper.UnindexAccountOpenOrder(ctx, o)
}

func (m msgServer) CancelAllOrders(ctx context.Context, msg *types.MsgCancelAllOrders) (*types.MsgCancelAllOrdersResponse, error) {
	if ok, err := m.accountKeeper.IsAuthorized(ctx, msg.Sender, msg.AccountIndex); err != nil {
		return nil, err
	} else if !ok {
		return nil, types.ErrUnauthorized
	}
	// Only ImmediateCancelAll is implemented. Scheduled/abort variants
	// are not safe to claim success for until their state machine is
	// fully wired, so the handler now returns an explicit error rather
	// than lying to callers.
	if msg.Mode != perptypes.ImmediateCancelAll {
		return nil, types.ErrUnimplemented.Wrapf(
			"cancel-all mode=%d not supported", msg.Mode,
		)
	}
	params, err := m.Params.Get(ctx)
	if err != nil {
		return nil, err
	}
	maxCancels := params.MaxCancelsPerMsg
	if maxCancels == 0 {
		maxCancels = 128
	}
	// Collect first; the AccountOpenOrders iterator does not tolerate
	// writes during iteration. The index covers every resting order
	// regardless of client_order_index, and `MarketIndexFilter==0`
	// means all markets per the proto contract.
	targets := make([]orderbooktypes.Order, 0, maxCancels)
	if err := m.bookKeeper.IterateAccountOpenOrders(ctx, msg.AccountIndex, msg.MarketIndexFilter, func(o orderbooktypes.Order) bool {
		if uint32(len(targets)) >= maxCancels {
			return true
		}
		switch o.Status {
		case perptypes.OrderStatusOpen,
			perptypes.OrderStatusPartiallyFilled,
			perptypes.OrderStatusTriggeredPending:
			targets = append(targets, o)
		}
		return false
	}); err != nil {
		return nil, err
	}
	for _, o := range targets {
		if err := m.cancelOrderInternal(ctx, o); err != nil {
			return nil, err
		}
	}
	return &types.MsgCancelAllOrdersResponse{}, nil
}

func (m msgServer) ModifyOrder(ctx context.Context, msg *types.MsgModifyOrder) (*types.MsgModifyOrderResponse, error) {
	o, err := m.bookKeeper.GetOrder(ctx, msg.OrderIndex)
	if err != nil {
		return nil, err
	}
	if ok, err := m.accountKeeper.IsAuthorized(ctx, msg.Sender, o.OwnerAccountIndex); err != nil {
		return nil, err
	} else if !ok {
		return nil, types.ErrUnauthorized
	}
	// Only resting, modifiable orders can be modified.
	switch o.Status {
	case perptypes.OrderStatusOpen, perptypes.OrderStatusPartiallyFilled:
		if o.RemainingBaseAmount == 0 {
			return nil, types.ErrOrderNotCancelable.Wrapf("order_index=%d already fully filled", o.OrderIndex)
		}
	default:
		return nil, types.ErrOrderNotCancelable.Wrapf("order_index=%d status=%d", o.OrderIndex, o.Status)
	}
	if err := m.cancelOrderInternal(ctx, o); err != nil {
		return nil, err
	}

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
