package types

// Event types emitted by the orderbook module.
const (
	// EventTypeExpirySweepError is emitted by EndBlocker when an
	// individual GTT-cancel fails with an error other than
	// ErrOrderNotCancelable. We continue sweeping the rest of the
	// expired set instead of letting one corrupt entry abort the
	// block.
	EventTypeExpirySweepError = "orderbook_expiry_sweep_error"
)

// Attribute keys for orderbook events.
const (
	AttributeKeyOrderIndex = "order_index"
	AttributeKeyErr        = "err"
)
