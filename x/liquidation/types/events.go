package types

// Event types for the liquidation module.
const (
	EventTypeLiquidate          = "liquidate"
	EventTypeMarketExitPosition = "market_exit_position"
	EventTypeLiquidationFlagged = "liquidation_flagged"
	EventTypeAutoADL            = "auto_adl"
)

// Attribute keys for liquidation events.
const (
	AttributeKeyVictim          = "victim"
	AttributeKeyMarketIndex     = "market_index"
	AttributeKeyBaseAmount      = "base_amount"
	AttributeKeyZeroPrice       = "zero_price"
	AttributeKeyClosePrice      = "close_price"
	AttributeKeyClosedPositions = "closed_positions"
	AttributeKeyAccountIndex    = "account_index"
	AttributeKeyStatus          = "status"
	AttributeKeyCounterparty    = "counterparty"
	AttributeKeyPrice           = "price"
	AttributeKeyVictimZeroPrice = "victim_zero_price"
	AttributeKeyCandZeroPrice   = "cand_zero_price"
)
