package keeper

import (
	"context"

	"cosmossdk.io/math"
	sdk "github.com/cosmos/cosmos-sdk/types"

	perptypes "github.com/perpdex/perpdex-l1/types"
	markettypes "github.com/perpdex/perpdex-l1/x/market/types"
	"github.com/perpdex/perpdex-l1/x/matching/types"
	orderbooktypes "github.com/perpdex/perpdex-l1/x/orderbook/types"
)

// spotResidueLock mirrors x/orderbook computeSpotLock: it returns the
// (asset_id, amount) the spot residue would need to lock once it rests
// on the book.
func spotResidueLock(o orderbooktypes.Order, m markettypes.Market) (uint32, math.Int) {
	if o.IsAsk {
		return m.BaseAssetId, math.NewIntFromUint64(o.RemainingBaseAmount)
	}
	notional := math.NewIntFromUint64(o.RemainingBaseAmount).
		Mul(math.NewIntFromUint64(uint64(o.Price)))
	return m.QuoteAssetId, notional
}

type MsgServer struct{ Keeper }

func NewMsgServerImpl(k Keeper) types.MsgServer { return &MsgServer{Keeper: k} }

var _ types.MsgServer = MsgServer{}

// IsTriggerOrder is a local predicate replicating the one in msgs.go so
// keeper code can avoid importing types.isTriggerOrderType.
func IsTriggerOrder(t uint32) bool {
	return t == perptypes.StopLossOrder ||
		t == perptypes.StopLossLimitOrder ||
		t == perptypes.TakeProfitOrder ||
		t == perptypes.TakeProfitLimitOrder
}

// QuoteExceedsLimit returns true when base * price exceeds the market's
// configured `OrderQuoteLimit`. The multiplication is done over `math.Int`
// (arbitrary-precision) so overflow can never wrap a malicious order back
// under the cap — even with `base ~ 2^48` and `price ~ 2^32` legal inputs.
func QuoteExceedsLimit(base uint64, price uint32, limit int64) bool {
	if limit <= 0 || base == 0 || price == 0 {
		return false
	}
	prod := math.NewIntFromUint64(base).Mul(math.NewIntFromUint64(uint64(price)))
	return prod.GT(math.NewInt(limit))
}

// willConsumeOpenSlot reports whether `msg` could plausibly leave a
// resting / trigger-pending order on the book once handled. IOC orders
// never consume an open slot (they are cancelled if they cannot fully
// match), but every other variant either rests on the book or registers
// a trigger, so they must respect the per-account cap.
func willConsumeOpenSlot(msg *types.MsgCreateOrder) bool {
	if IsTriggerOrder(msg.OrderType) {
		return true
	}
	return msg.TimeInForce != perptypes.IOC
}

func (m MsgServer) CreateOrder(ctx context.Context, msg *types.MsgCreateOrder) (*types.MsgCreateOrderResponse, error) {
	if err := msg.ValidateBasic(); err != nil {
		return nil, err
	}
	if ok, err := m.accountKeeper.IsAuthorized(ctx, msg.Sender, msg.AccountIndex); err != nil {
		return nil, err
	} else if !ok {
		return nil, types.ErrUnauthorized
	}
	// Public pools and the Insurance Fund cannot place orders
	// directly; their fills come exclusively from the liquidation /
	// ADL paths. The order-creation path rejects
	// PUBLIC_POOL_ACCOUNT_TYPE and INSURANCE_FUND_ACCOUNT_TYPE up
	// front.
	if acc, err := m.accountKeeper.GetAccount(ctx, msg.AccountIndex); err == nil {
		if acc.IsPoolType() {
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
	// Pre-liquidation order placement gate. Rule:
	//   - HEALTHY: any order allowed.
	//   - PRE_LIQUIDATION: only reduce-only orders that strictly shrink
	//     the account's position in this market are allowed. We
	//     additionally require the cross account (or, for an isolated
	//     trade, that specific isolated position) to be in PRE — not
	//     a deeper liquidation tier.
	//   - PARTIAL / FULL / BANKRUPTCY: reject every user-initiated
	//     order. The liquidation engine is the only writer in these
	//     tiers; any user trade would race liquidation fills.
	if err := m.CheckPreLiquidationGate(ctx, msg.AccountIndex, msg.MarketIndex, msg.ReduceOnly); err != nil {
		return nil, err
	}
	if market.MinBaseAmount > 0 && msg.BaseAmount < market.MinBaseAmount {
		return nil, types.ErrInvalidOrder.Wrapf("base_amount %d below market min %d", msg.BaseAmount, market.MinBaseAmount)
	}
	if market.MinQuoteAmount > 0 && msg.Price > 0 {
		if uint64(msg.Price)*msg.BaseAmount < market.MinQuoteAmount {
			return nil, types.ErrInvalidOrder.Wrapf("quote notional below market min %d", market.MinQuoteAmount)
		}
	}
	if QuoteExceedsLimit(msg.BaseAmount, msg.Price, market.OrderQuoteLimit) {
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

	// Per-market open-order cap: bound the number of resting +
	// trigger-pending orders per account so an adversary cannot
	// exhaust the orderbook with free post-only orders. The cap is
	// enforced ahead of the matching pass so an order that would
	// clearly violate the cap (e.g. plain non-IOC limit, or any
	// trigger order which immediately reserves a slot) is rejected
	// up front. Pure IOC and POST_ONLY-that-fully-matches orders that
	// happen to consume zero slots are still allowed even when the
	// account is at the cap; we re-check once the residual is known
	// before resting.
	if market.MaxOpenOrdersPerAccount > 0 && willConsumeOpenSlot(msg) {
		count, err := m.bookKeeper.GetAccountOpenOrderCount(ctx, msg.AccountIndex, msg.MarketIndex)
		if err != nil {
			return nil, err
		}
		if count >= market.MaxOpenOrdersPerAccount {
			return nil, types.ErrTooManyOpenOrders.Wrapf(
				"account=%d market=%d count=%d cap=%d",
				msg.AccountIndex, msg.MarketIndex, count, market.MaxOpenOrdersPerAccount,
			)
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
	// price crossover activates them in EndBlocker. OpenTriggerOrder
	// owns the trigger registration plus client and account-open
	// indexes so cancel-all can still reach a pending trigger.
	if IsTriggerOrder(msg.OrderType) {
		order.Status = perptypes.OrderStatusTriggeredPending
		order.TriggerStatus = perptypes.TriggerStatusMarkPrice
		if err := m.bookKeeper.OpenTriggerOrder(ctx, order); err != nil {
			return nil, err
		}
		return &types.MsgCreateOrderResponse{OrderIndex: idx, Status: order.Status}, nil
	}

	params, err := m.Params.Get(ctx)
	if err != nil {
		return nil, err
	}
	filled, status, err := m.MatchOrder(ctx, &order, params.MaxFillsPerMsg)
	if err != nil {
		return nil, err
	}
	order.Status = status

	if msg.TimeInForce == perptypes.IOC && order.RemainingBaseAmount > 0 {
		order.Status = perptypes.OrderStatusCancelled
	}
	// Spot pre-rest balance gate: if the residue cannot be locked in
	// full (Available < required), force-cancel the residue rather
	// than letting OpenOrder fail with ErrInsufficientFunds (which
	// would revert the whole Msg and lose already-committed fills).
	// Already-landed fills from the matching pass survive (they
	// reside in writeCache); only the residue must be cancelled here.
	if (order.Status == perptypes.OrderStatusOpen || order.Status == perptypes.OrderStatusPartiallyFilled) &&
		market.MarketType == perptypes.MarketTypeSpot &&
		order.RemainingBaseAmount > 0 {
		assetID, lockAmt := spotResidueLock(order, market)
		avail, err := m.accountKeeper.AvailableBalance(ctx, order.OwnerAccountIndex, assetID)
		if err != nil {
			return nil, err
		}
		if avail.LT(lockAmt) {
			order.Status = perptypes.OrderStatusCancelled
			sdk.UnwrapSDKContext(ctx).Logger().Error(
				"matching CreateOrder: spot residue unlockable; force-cancelled residue",
				"market_index", order.MarketIndex,
				"order_index", order.OrderIndex,
				"asset_id", assetID,
				"available", avail.String(),
				"required", lockAmt.String(),
			)
		}
	}
	if err := m.bookKeeper.OpenOrder(ctx, order); err != nil {
		return nil, err
	}

	return &types.MsgCreateOrderResponse{
		OrderIndex:       idx,
		Status:           order.Status,
		FilledBaseAmount: filled,
	}, nil
}

// CheckPreLiquidationGate enforces the pre-liquidation order placement
// rule from the spec. The check happens BEFORE we touch the orderbook
// so a frozen / unhealthy account cannot use CreateOrder /
// ModifyOrder to interleave with the liquidation engine.
//
// The gate consults the cross account health AND, when the touched
// market hosts an isolated position for this account, the per-market
// isolated health. Either being unhealthy is enough to reject.
func (m MsgServer) CheckPreLiquidationGate(ctx context.Context, accIdx uint64, marketIdx uint32, reduceOnly bool) error {
	if m.riskKeeper == nil {
		// Risk keeper not wired (tests / staged genesis): skip the
		// gate rather than panic.
		return nil
	}
	cross, err := m.riskKeeper.GetHealthStatus(ctx, accIdx)
	if err != nil {
		return err
	}
	iso, err := m.riskKeeper.GetIsolatedHealthStatus(ctx, accIdx, marketIdx)
	if err != nil {
		return err
	}
	worst := cross
	if iso > worst {
		worst = iso
	}
	switch worst {
	case perptypes.HealthHealthy:
		return nil
	case perptypes.HealthPreLiquidation:
		if !reduceOnly {
			return types.ErrAccountUnderLiquidation.Wrapf(
				"account=%d in pre-liquidation; only reduce-only orders allowed", accIdx,
			)
		}
		return nil
	default:
		return types.ErrAccountUnderLiquidation.Wrapf(
			"account=%d health=%d; user orders blocked", accIdx, worst,
		)
	}
}

// reduceOnlyCompatible reports whether a reduce-only order on `isAsk` side
// with `baseAmount` size can legitimately only reduce the account's current
// position: the account must be net-positive/negative on the opposite side.
func (m MsgServer) reduceOnlyCompatible(ctx context.Context, accIdx uint64, marketIdx uint32, isAsk bool, _ uint64) (bool, error) {
	pos, err := m.accountKeeper.GetPosition(ctx, accIdx, marketIdx)
	if err != nil {
		return false, err
	}
	if pos.BaseSize.IsZero() {
		return false, nil
	}
	// Taker ask (seller) must be long; taker bid (buyer) must be short.
	if isAsk {
		return pos.OpeningIsBid(), nil
	}
	return pos.OpeningIsAsk(), nil
}

func (m MsgServer) CancelOrder(ctx context.Context, msg *types.MsgCancelOrder) (*types.MsgCancelOrderResponse, error) {
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
	if err := m.cancelOrderInternal(ctx, o); err != nil {
		return nil, err
	}
	return &types.MsgCancelOrderResponse{}, nil
}

// cancelOrderInternal is the shared cancel path used by CancelOrder,
// CancelAllOrders and ModifyOrder. The state-machine, entry/trigger
// removal, and index cleanup are all owned by orderbook.CancelOrder.
func (m MsgServer) cancelOrderInternal(ctx context.Context, o orderbooktypes.Order) error {
	_, err := m.bookKeeper.CancelOrder(ctx, o.OrderIndex)
	return err
}

func (m MsgServer) CancelAllOrders(ctx context.Context, msg *types.MsgCancelAllOrders) (*types.MsgCancelAllOrdersResponse, error) {
	if err := msg.ValidateBasic(); err != nil {
		return nil, err
	}
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
	if err := m.bookKeeper.IterateAccountOpenOrders(ctx, msg.AccountIndex, msg.MarketIndexFilter, func(o orderbooktypes.Order) error {
		if uint32(len(targets)) >= maxCancels {
			return orderbooktypes.ErrStopIteration
		}
		switch o.Status {
		case perptypes.OrderStatusOpen,
			perptypes.OrderStatusPartiallyFilled,
			perptypes.OrderStatusTriggeredPending:
			targets = append(targets, o)
		}
		return nil
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

func (m MsgServer) ModifyOrder(ctx context.Context, msg *types.MsgModifyOrder) (*types.MsgModifyOrderResponse, error) {
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
	switch o.Status {
	case perptypes.OrderStatusOpen, perptypes.OrderStatusPartiallyFilled:
		if o.RemainingBaseAmount == 0 {
			return nil, types.ErrOrderNotCancelable.Wrapf("order_index=%d already fully filled", o.OrderIndex)
		}
	default:
		return nil, types.ErrOrderNotCancelable.Wrapf("order_index=%d status=%d", o.OrderIndex, o.Status)
	}
	// Apply the same pre-liquidation gate as CreateOrder before we
	// touch the book. ModifyOrder is cancel-then-create; without this
	// check a PARTIAL account could blow away its open orders during
	// liquidation by repeatedly issuing a no-op modify.
	if err := m.CheckPreLiquidationGate(ctx, o.OwnerAccountIndex, o.MarketIndex, o.ReduceOnly); err != nil {
		return nil, err
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

func (m MsgServer) UpdateParams(ctx context.Context, msg *types.MsgUpdateParams) (*types.MsgUpdateParamsResponse, error) {
	if err := msg.ValidateBasic(); err != nil {
		return nil, err
	}
	if msg.Authority != m.authority {
		return nil, types.ErrInvalidAuthority
	}
	if err := m.Params.Set(ctx, msg.Params); err != nil {
		return nil, err
	}
	return &types.MsgUpdateParamsResponse{}, nil
}
