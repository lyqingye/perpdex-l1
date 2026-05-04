package keeper

import (
	"context"
	"errors"
	"fmt"

	"cosmossdk.io/collections"
	"cosmossdk.io/core/store"

	"github.com/cosmos/cosmos-sdk/codec"

	"github.com/perpdex/perpdex-l1/x/orderbook/types"
)

// Keeper holds the orderbook state.
type Keeper struct {
	cdc          codec.BinaryCodec
	storeService store.KVStoreService
	authority    string

	marketKeeper types.MarketKeeper

	Schema collections.Schema
	Params collections.Item[types.Params]

	// Orders[order_index] = Order.
	Orders         collections.Map[uint64, types.Order]
	NextOrderIndex collections.Sequence

	// OrderBookEntries[(market, side_and_sort_key)] = OrderBookEntry.
	// The bytes part is layout: 1 byte side (0=ask, 1=bid) + 12 bytes sortable
	// key. Stored together so a per-side iterator is just a prefixed range.
	OrderBookEntries collections.Map[collections.Pair[uint32, []byte], types.OrderBookEntry]

	// OrderToSortKey[(market, order_index)] = sortable_key. Used to remove an
	// order without re-deriving the key.
	OrderToSortKey collections.Map[collections.Pair[uint32, uint64], []byte]

	// PriceLevels[(market, price)] = PriceLevelAggregate. Aggregates the depth
	// at a given price level, used for impact price computation.
	PriceLevels collections.Map[collections.Pair[uint32, uint32], types.PriceLevelAggregate]

	// UserOrderIndex[(market, account, client_order_index)] = order_index. Used
	// to look up an order by client id.
	UserOrderIndex collections.Map[collections.Triple[uint32, uint64, uint64], uint64]

	// TriggerIndex tracks orders awaiting trigger price activation.
	TriggerIndex collections.KeySet[collections.Triple[uint32, uint32, uint64]]

	// AccountOpenOrders[(account, order_index)]: tracks every order
	// belonging to `account` that is in a non-terminal status (open /
	// partially_filled / triggered_pending). Independent of
	// client_order_index so cancel-all can find every resting order.
	AccountOpenOrders collections.KeySet[collections.Pair[uint64, uint64]]
}

func NewKeeper(cdc codec.BinaryCodec, storeService store.KVStoreService, authority string, mk types.MarketKeeper) Keeper {
	sb := collections.NewSchemaBuilder(storeService)
	k := Keeper{
		cdc:          cdc,
		storeService: storeService,
		authority:    authority,
		marketKeeper: mk,

		Params:         collections.NewItem(sb, types.ParamsKey, "params", codec.CollValue[types.Params](cdc)),
		Orders:         collections.NewMap(sb, types.OrderKey, "orders", collections.Uint64Key, codec.CollValue[types.Order](cdc)),
		NextOrderIndex: collections.NewSequence(sb, types.NextOrderIndexKey, "next_order_index"),

		OrderBookEntries: collections.NewMap(sb, types.OrderBookEntryKey, "ob_entries",
			collections.PairKeyCodec(collections.Uint32Key, collections.BytesKey),
			codec.CollValue[types.OrderBookEntry](cdc)),

		OrderToSortKey: collections.NewMap(sb, types.OrderToSortKey, "order_to_sort_key",
			collections.PairKeyCodec(collections.Uint32Key, collections.Uint64Key),
			collections.BytesValue),

		PriceLevels: collections.NewMap(sb, types.PriceLevelKey, "price_levels",
			collections.PairKeyCodec(collections.Uint32Key, collections.Uint32Key),
			codec.CollValue[types.PriceLevelAggregate](cdc)),

		UserOrderIndex: collections.NewMap(sb, types.UserOrderIndexKey, "user_order_index",
			collections.TripleKeyCodec(collections.Uint32Key, collections.Uint64Key, collections.Uint64Key),
			collections.Uint64Value),

		TriggerIndex: collections.NewKeySet(sb, types.TriggerIndexKey, "trigger_index",
			collections.TripleKeyCodec(collections.Uint32Key, collections.Uint32Key, collections.Uint64Key)),

		AccountOpenOrders: collections.NewKeySet(sb, types.AccountOpenOrdersKey, "account_open_orders",
			collections.PairKeyCodec(collections.Uint64Key, collections.Uint64Key)),
	}
	schema, err := sb.Build()
	if err != nil {
		panic(fmt.Errorf("orderbook: %w", err))
	}
	k.Schema = schema
	return k
}

func (k Keeper) Authority() string { return k.authority }

// GetOrder returns the order at order_index.
func (k Keeper) GetOrder(ctx context.Context, idx uint64) (types.Order, error) {
	o, err := k.Orders.Get(ctx, idx)
	if err != nil {
		if errors.Is(err, collections.ErrNotFound) {
			return types.Order{}, types.ErrOrderNotFound.Wrapf("order_index=%d", idx)
		}
		return types.Order{}, err
	}
	return o, nil
}

// SetOrder stores the order keyed by order_index.
func (k Keeper) SetOrder(ctx context.Context, o types.Order) error {
	return k.Orders.Set(ctx, o.OrderIndex, o)
}

// GetOrderByClientID looks up the canonical order_index for a (market, account,
// client_order_index) tuple.
func (k Keeper) GetOrderByClientID(ctx context.Context, market uint32, account uint64, clientID uint64) (types.Order, error) {
	idx, err := k.UserOrderIndex.Get(ctx, collections.Join3(market, account, clientID))
	if err != nil {
		if errors.Is(err, collections.ErrNotFound) {
			return types.Order{}, types.ErrOrderNotFound
		}
		return types.Order{}, err
	}
	return k.GetOrder(ctx, idx)
}
