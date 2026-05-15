package types

const (
	EventTypeLiquidate          = "liquidate"
	EventTypeMarketExitPosition = "market_exit_position"
	EventTypeAutoADL            = "auto_adl"
	// EventTypeDeleverage is emitted at the end of every successful
	// `Deleverage` call regardless of entry point (MsgDeleverage,
	// LLP / IF absorb, autoADL). The `source` attribute distinguishes
	// the three paths so downstream indexers can audit user-driven vs
	// system-driven deleverages without correlating against ADL /
	// LLP-specific events.
	EventTypeDeleverage = "deleverage"
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
	AttributeKeyDeleverager     = "deleverager"
	// AttributeKeySource tags the entry-point on `EventTypeDeleverage`
	// (msg / llp / auto_adl) so a single event stream is sufficient to
	// audit every deleverage path without joining against ADL / LLP
	// specific events.
	AttributeKeySource = "source"
)
