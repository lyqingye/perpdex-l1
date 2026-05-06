package types

// Event types for the matching module.
const (
	EventTypeOrderFill              = "order_fill"
	EventTypeMakerEvictedBadState   = "maker_evicted_bad_state"
	EventTypeTakerAbortedBadState   = "taker_aborted_bad_state"
	EventTypeOrderResidueUnlockable = "order_residue_unlockable"
	EventTypeTriggerOracleError     = "trigger_oracle_error"
	EventTypeTriggerDequeueError    = "trigger_dequeue_error"
	EventTypeTriggerMatchError      = "trigger_match_error"
	EventTypeTriggerInsertError     = "trigger_insert_error"
)

// Attribute keys for matching events.
const (
	AttributeKeyMarketIndex = "market_index"
	AttributeKeyOrderIndex  = "order_index"
	AttributeKeyPrice       = "price"
	AttributeKeyBase        = "base"
	AttributeKeyReason      = "reason"
	AttributeKeyAssetID     = "asset_id"
	AttributeKeyAvailable   = "available"
	AttributeKeyRequired    = "required"
	AttributeKeyErr         = "err"
)
