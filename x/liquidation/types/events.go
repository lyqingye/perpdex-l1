package types

const (
	EventTypeLiquidate          = "liquidate"
	EventTypeMarketExitPosition = "market_exit_position"
	EventTypeAutoADL            = "auto_adl"
)

const (
	AttributeKeyVictim          = "victim"
	AttributeKeyMarketIndex     = "market_index"
	AttributeKeyBaseAmount      = "base_amount"
	AttributeKeyZeroPrice       = "zero_price"
	AttributeKeyClosePrice      = "close_price"
	AttributeKeyClosedPositions = "closed_positions"
	AttributeKeyCounterparty    = "counterparty"
	AttributeKeyPrice           = "price"
	AttributeKeyVictimZeroPrice = "victim_zero_price"
	AttributeKeyCandZeroPrice   = "cand_zero_price"
)
