package types

// Event types for the matching module. Only state-change events that
// downstream consumers (indexers, UIs) need to observe live here;
// internal diagnostics (trigger-sweep failures, maker eviction on
// recoverable error, taker abort, spot residue force-cancel) are
// surfaced via the keeper logger rather than the event bus.
const (
	EventTypeOrderFill = "order_fill"
)

// Attribute keys for matching events.
const (
	AttributeKeyMarketIndex = "market_index"
	AttributeKeyPrice       = "price"
	AttributeKeyBase        = "base"
)
