package types

// Event types for the funding module.
const (
	EventTypeFundingSampleError = "funding_sample_error"
	EventTypeFundingSettleError = "funding_settle_error"
)

// Attribute keys for funding events.
const (
	AttributeKeyMarketIndex = "market_index"
	AttributeKeyErr         = "err"
)
