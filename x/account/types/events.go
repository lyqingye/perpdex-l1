package types

// Event types for the account module.
const (
	EventTypeDeposit          = "deposit"
	EventTypeCreatePublicPool = "create_public_pool"
	EventTypeUpdatePublicPool = "update_public_pool"
	EventTypeMintShares       = "mint_shares"
	EventTypeBurnShares       = "burn_shares"
	EventTypeStrategyTransfer = "strategy_transfer"
)

// Attribute keys for account events.
const (
	AttributeKeyAccountIndex       = "account_index"
	AttributeKeyAssetIndex         = "asset_index"
	AttributeKeyAmount             = "amount"
	AttributeKeyRoute              = "route"
	AttributeKeyPoolAccountIndex   = "pool_account_index"
	AttributeKeyMasterAccountIndex = "master_account_index"
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
)
