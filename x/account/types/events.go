package types

// Event types for the account module.
const (
	EventTypeDeposit                 = "deposit"
	EventTypeWithdraw                = "withdraw"
	EventTypeTransfer                = "transfer"
	EventTypeCreateSubAccount        = "create_sub_account"
	EventTypeUpdateAccountConfig     = "update_account_config"
	EventTypeUpdateAccountAssetConfig = "update_account_asset_config"
	EventTypeUpdateMargin            = "update_margin"
	EventTypeUpdateLeverage          = "update_leverage"
	EventTypeCreatePublicPool        = "create_public_pool"
	EventTypeUpdatePublicPool        = "update_public_pool"
	EventTypeMintShares              = "mint_shares"
	EventTypeBurnShares              = "burn_shares"
	EventTypeStrategyTransfer        = "strategy_transfer"
)

// Attribute keys for account events.
const (
	AttributeKeyAccountIndex       = "account_index"
	AttributeKeyAssetIndex         = "asset_index"
	AttributeKeyAmount             = "amount"
	AttributeKeyRoute              = "route"
	AttributeKeyPoolAccountIndex   = "pool_account_index"
	AttributeKeyMasterAccountIndex = "master_account_index"
	AttributeKeySubAccountIndex    = "sub_account_index"
	AttributeKeyAccountType        = "account_type"
	AttributeKeyInitialTotalShares = "initial_total_shares"
	AttributeKeyStatus             = "status"
	AttributeKeyOperatorFee        = "operator_fee"
	AttributeKeySenderMaster       = "sender_master"
	AttributeKeyShareAmount        = "share_amount"
	AttributeKeyPrincipalAmount    = "principal_amount"
	AttributeKeyDepositor          = "depositor"
	AttributeKeyOperatorFeeShares  = "operator_fee_shares"
	AttributeKeyUsdcAmount         = "usdc_amount"
	AttributeKeyFrom               = "from"
	AttributeKeyTo                 = "to"
	AttributeKeyFromAccountIndex   = "from_account_index"
	AttributeKeyToAccountIndex     = "to_account_index"
	AttributeKeyMarketIndex        = "market_index"
	AttributeKeyAction             = "action"
	AttributeKeyMarginMode         = "margin_mode"
	AttributeKeyInitialMarginFraction = "initial_margin_fraction"
	AttributeKeyTradingMode        = "trading_mode"
)
