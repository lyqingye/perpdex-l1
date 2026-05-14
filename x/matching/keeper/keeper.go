package keeper

import (
	"context"
	"fmt"

	"cosmossdk.io/collections"
	"cosmossdk.io/core/store"

	"github.com/cosmos/cosmos-sdk/codec"

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

// SetRiskKeeper wires the risk keeper after construction. Risk is needed
// for the pre-liquidation order placement gate; it is wired late to
// avoid the import cycle that would otherwise arise from x/risk
// depending on x/matching for cancel-all in liquidation paths.
func (k *Keeper) SetRiskKeeper(r types.RiskKeeper) { k.riskKeeper = r }

// SetTradeKeeper swaps the trade keeper at runtime. Production wiring
// always passes the keeper through `NewKeeper`; this setter exists so
// external test packages can inject a fake trade keeper (e.g. for
// error-injection / matching-recovery tests) without exposing the
// underlying field.
func (k *Keeper) SetTradeKeeper(t types.TradeKeeper) { k.tradeKeeper = t }

// CancelAllOpenOrdersForAccount cancels every resting order owned by
// `accountIdx` across every market, bypassing sender authority checks.
// Reserved for the liquidation engine: per the spec the partial
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
	if err := k.bookKeeper.IterateAccountOpenOrders(ctx, accountIdx, 0, func(o orderbooktypes.Order) error {
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
		return 0, err
	}
	cancelled := uint32(0)
	for _, o := range targets {
		if _, err := k.bookKeeper.CancelOrder(ctx, o.OrderIndex); err != nil {
			return cancelled, err
		}
		cancelled++
	}
	return cancelled, nil
}

func (k Keeper) Authority() string { return k.authority }
