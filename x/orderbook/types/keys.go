package types

const (
	ModuleName = "orderbook"
	StoreKey   = ModuleName
	RouterKey  = ModuleName
)

// DefaultOrderBookSnapshotMaxDepth caps the per-side levels returned by
// the OrderBookSnapshot query. The cap protects the public RPC from
// accidental full-book pulls that would dominate validator query CPU
// on busy markets; callers that need deeper context should iterate
// price levels directly via state queries.
const DefaultOrderBookSnapshotMaxDepth uint32 = 50

var (
	ParamsKey                = []byte{0x00}
	OrderBookEntryKey        = []byte{0x01}
	OrderToSortKey           = []byte{0x02}
	PriceLevelKey            = []byte{0x03}
	UserOrderIndexKey        = []byte{0x04}
	TriggerIndexKey          = []byte{0x05}
	OrderKey                 = []byte{0x06}
	NextOrderIndexKey        = []byte{0x07}
	AccountOpenOrdersKey     = []byte{0x08}
	AccountOpenOrderCountKey = []byte{0x09}
	// ExpiryIndexKey backs the GTT expiry keyset
	// `(expiry_ms, order_index) -> ()`. EndBlocker iterates this
	// keyset by ascending expiry so each block walks only the
	// orders due to expire, never the full Orders history.
	ExpiryIndexKey = []byte{0x0A}
)
