package keeper

import (
	"context"
	"fmt"

	"cosmossdk.io/collections"
	"cosmossdk.io/core/store"

	"github.com/cosmos/cosmos-sdk/codec"
	sdk "github.com/cosmos/cosmos-sdk/types"

	perptypes "github.com/perpdex/perpdex-l1/types"
	"github.com/perpdex/perpdex-l1/x/matching/types"
	orderbooktypes "github.com/perpdex/perpdex-l1/x/orderbook/types"
)

type Keeper struct {
	cdc           codec.BinaryCodec
	storeService  store.KVStoreService
	authority     string
	accountKeeper types.AccountKeeper
	marketKeeper  types.MarketKeeper
	bookKeeper    types.OrderbookKeeper
	tradeKeeper   types.TradeKeeper
	oracleKeeper  types.OracleKeeper
	riskKeeper    types.RiskKeeper

	Schema collections.Schema
	Params collections.Item[types.Params]
}

func NewKeeper(cdc codec.BinaryCodec, storeService store.KVStoreService, authority string,
	ak types.AccountKeeper, mk types.MarketKeeper, bk types.OrderbookKeeper, tk types.TradeKeeper,
) Keeper {
	sb := collections.NewSchemaBuilder(storeService)
	k := Keeper{
		cdc:           cdc,
		storeService:  storeService,
		authority:     authority,
		accountKeeper: ak,
		marketKeeper:  mk,
		bookKeeper:    bk,
		tradeKeeper:   tk,

		Params: collections.NewItem(sb, types.ParamsKey, "params", codec.CollValue[types.Params](cdc)),
	}
	schema, err := sb.Build()
	if err != nil {
		panic(fmt.Errorf("matching: %w", err))
	}
	k.Schema = schema
	return k
}

// SetOracleKeeper wires the oracle keeper after construction. Required for
// EndBlocker trigger resolution; the keeper is oracle-agnostic at NewKeeper
// time to avoid import cycles with modules that depend on matching.
func (k *Keeper) SetOracleKeeper(o types.OracleKeeper) { k.oracleKeeper = o }

// SetRiskKeeper wires the risk keeper after construction. Risk is needed
// for the pre-liquidation order placement gate; it is wired late to
// avoid the import cycle that would otherwise arise from x/risk
// depending on x/matching for cancel-all in liquidation paths.
func (k *Keeper) SetRiskKeeper(r types.RiskKeeper) { k.riskKeeper = r }

// MatchLiquidationOrder is the system-only entry point used by the
// liquidation keeper to drive a partial-liquidation close-out through
// the public orderbook (Lighter parity with `InternalLiquidatePositionTx`
// + `LIQUIDATION_ORDER + IOC + reduce_only` flow).
//
// The synthetic taker is owned by the victim. It is constructed in-
// memory only — never persisted via `OpenOrder`, never indexed against
// the account-open / client-id maps, and never counted against the
// per-account open-order cap. IOC residue is silently discarded by
// returning the partial `filled` count.
//
// The caller is expected to have already cancelled every resting
// order owned by the victim (via `CancelAllOpenOrdersForAccount`)
// before invoking this entry point so a victim's own bids cannot
// front-run the close-out fill (matching Lighter's
// `InternalCancelAllOrdersTx → InternalLiquidatePositionTx` ordering).
//
// Side direction is derived from the victim's current position:
// long victim → sell to close (IsAsk=true), short victim → buy to
// close (IsAsk=false). The order is reduce-only, so the matching loop
// will cap each fill against the victim's residual position size and
// can never accidentally flip the account to the opposite side.
//
// `liquidationFeeBps` is the market's configured liquidation fee
// (passed through the trade engine's improvement-over-zero-price
// formula); `liquidationFeeRecipient` is typically the Insurance Fund
// operator account but is left explicit so future LLP-targeted
// recipients can be wired without touching this surface.
func (k Keeper) MatchLiquidationOrder(
	ctx context.Context,
	victim uint64,
	marketIdx uint32,
	zeroPrice uint32,
	baseAmount uint64,
	liquidationFeeBps uint32,
	liquidationFeeRecipient uint64,
) (uint64, error) {
	if baseAmount == 0 {
		return 0, types.ErrInvalidOrder.Wrap("liquidation base_amount must be > 0")
	}
	if zeroPrice == 0 {
		return 0, types.ErrInvalidOrder.Wrap("liquidation zero price must be > 0")
	}
	pos, err := k.accountKeeper.GetPosition(ctx, victim, marketIdx)
	if err != nil {
		return 0, err
	}
	if pos.Position.IsZero() {
		return 0, types.ErrInvalidOrder.Wrapf("victim=%d has no position in market=%d", victim, marketIdx)
	}
	// Long victim closes via SELL (taker ask); short victim closes
	// via BUY (taker bid). Mirrors x/liquidation/keeper/liquidate.go
	// `takerIsAsk := pos.Position.IsNegative()` semantics — except
	// here the victim is the taker, so the sign flips.
	isAsk := pos.Position.IsPositive()
	// Cap requested base by victim's |position| so the synthetic
	// reduce-only IOC cannot ask for more than the close-out size.
	abs := pos.Position.Abs().Uint64()
	if baseAmount > abs {
		baseAmount = abs
	}

	params, err := k.Params.Get(ctx)
	if err != nil {
		return 0, err
	}
	maxFills := params.MaxFillsPerMsg
	if maxFills == 0 {
		maxFills = 64
	}

	now := sdk.UnwrapSDKContext(ctx).BlockTime().UnixMilli()
	taker := orderbooktypes.Order{
		// OrderIndex / Nonce intentionally left zero: the synthetic
		// taker is never persisted, never indexed, and never compared
		// against book entries via OrderIndex. The matching kernel only
		// reads MarketIndex / OwnerAccountIndex / IsAsk / OrderType /
		// Price / RemainingBaseAmount / ReduceOnly / Expiry from the
		// taker.
		OwnerAccountIndex:   victim,
		MarketIndex:         marketIdx,
		IsAsk:               isAsk,
		OrderType:           perptypes.LiquidationOrder,
		TimeInForce:         perptypes.IOC,
		ReduceOnly:          true,
		Price:               zeroPrice,
		InitialBaseAmount:   baseAmount,
		RemainingBaseAmount: baseAmount,
		CreatedAt:           now,
		Status:              perptypes.OrderStatusOpen,
	}
	filled, _, err := k.matchLiquidationLoop(
		ctx, &taker, maxFills,
		zeroPrice, liquidationFeeBps, liquidationFeeRecipient,
	)
	return filled, err
}

// CancelAllOpenOrdersForAccount cancels every resting order owned by
// `accountIdx` across every market, bypassing sender authority checks.
// Reserved for the liquidation engine: per Lighter spec the partial
// liquidation flow must clear the victim's book before issuing
// zero-price IoC closes, otherwise a victim's resting bids could
// front-run the liquidation fill.
func (k Keeper) CancelAllOpenOrdersForAccount(ctx context.Context, accountIdx uint64) (uint32, error) {
	params, err := k.Params.Get(ctx)
	if err != nil {
		return 0, err
	}
	maxCancels := params.MaxCancelsPerMsg
	if maxCancels == 0 {
		maxCancels = 128
	}
	targets := make([]orderbooktypes.Order, 0, maxCancels)
	if err := k.bookKeeper.IterateAccountOpenOrders(ctx, accountIdx, 0, func(o orderbooktypes.Order) bool {
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
		return 0, err
	}
	cancelled := uint32(0)
	for _, o := range targets {
		// orderbook.CancelOrder owns the state-machine, entry/trigger
		// removal, and index cleanup for cancellations across user,
		// liquidation, and EndBlocker call sites.
		if _, err := k.bookKeeper.CancelOrder(ctx, o.OrderIndex); err != nil {
			return cancelled, err
		}
		cancelled++
	}
	return cancelled, nil
}

func (k Keeper) Authority() string { return k.authority }
