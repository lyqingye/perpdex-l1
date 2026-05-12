package types

const (
	// EventTypeMarketCreated is emitted by MsgCreateMarket when a new
	// market is persisted. It carries enough metadata for indexers to
	// initialise their per-market caches without an extra round-trip.
	EventTypeMarketCreated = "market_created"

	// EventTypeMarketUpdated is emitted by MsgUpdateMarket after the
	// governance-managed fields on the static Market record are
	// overwritten. The attribute set mirrors the msg payload.
	EventTypeMarketUpdated = "market_updated"

	// EventTypeMarketDetailsUpdated is emitted by MsgUpdateMarketDetails.
	// Carries every field touched on MarketDetails so risk/funding
	// monitors can react without an extra query.
	EventTypeMarketDetailsUpdated = "market_details_updated"

	// EventTypeMarketParamsUpdated is emitted by MsgUpdateParams when
	// the chain-wide market Params change. Carries every Params field
	// so consumers can detect range/limit changes deterministically.
	EventTypeMarketParamsUpdated = "market_params_updated"

	// EventTypeMarketExpired is emitted whenever a market transitions
	// to MarketStatusExpired, whether via the EndBlocker auto-expiry
	// path or via a governance MsgUpdateMarket(NewStatus=Expired). Both
	// paths route through the keeper.expireMarket helper.
	EventTypeMarketExpired = "market_expired"

	// EventTypeMarketExpireExitFailed is emitted when expireMarket
	// successfully flips the market into EXPIRED state but the
	// LiquidationKeeper.ApplyExitPosition call to close out residual
	// positions fails (or the LiquidationKeeper is nil). The market is
	// still expired so trading halts; monitoring infra is expected to
	// raise an alert and operators must manually flatten the residual
	// positions before the insurance fund accrues bad debt.
	EventTypeMarketExpireExitFailed = "market_expire_exit_failed"
)

const (
	AttributeKeyMarketIndex          = "market_index"
	AttributeKeyMarketType           = "market_type"
	AttributeKeyBaseAssetID          = "base_asset_id"
	AttributeKeyQuoteAssetID         = "quote_asset_id"
	AttributeKeyCreatedAt            = "created_at"
	AttributeKeyExpiryTimestamp      = "expiry_timestamp"
	AttributeKeyStatus               = "status"
	AttributeKeyTakerFee             = "taker_fee"
	AttributeKeyMakerFee             = "maker_fee"
	AttributeKeyLiquidationFee       = "liquidation_fee"
	AttributeKeyMinBaseAmount        = "min_base_amount"
	AttributeKeyMinQuoteAmount       = "min_quote_amount"
	AttributeKeyOrderQuoteLimit      = "order_quote_limit"
	AttributeKeyDefaultImf           = "default_imf"
	AttributeKeyMinImf               = "min_imf"
	AttributeKeyMaintenanceMf        = "maintenance_mf"
	AttributeKeyCloseOutMf           = "close_out_mf"
	AttributeKeyFundingClampSmall    = "funding_clamp_small"
	AttributeKeyFundingClampBig      = "funding_clamp_big"
	AttributeKeyInterestRate         = "interest_rate"
	AttributeKeyOpenInterestLimit    = "open_interest_limit"
	AttributeKeyMaxPerpsMarketIndex  = "max_perps_market_index"
	AttributeKeyMinSpotMarketIndex   = "min_spot_market_index"
	AttributeKeyMaxSpotMarketIndex   = "max_spot_market_index"
	AttributeKeyMaxMarketsExpired    = "max_markets_expired_per_block"
	AttributeKeyExitError            = "exit_error"
)
