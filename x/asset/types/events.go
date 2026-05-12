package types

// Event types for the asset module.
const (
	EventTypeAssetRegistered = "asset_registered"
	EventTypeAssetUpdated    = "asset_updated"
	EventTypeParamsUpdated   = "asset_params_updated"
)

// Attribute keys for asset events.
const (
	AttributeKeyAssetIndex          = "asset_index"
	AttributeKeyDenom               = "denom"
	AttributeKeyDisplayName         = "display_name"
	AttributeKeyDecimals            = "decimals"
	AttributeKeyExtensionMultiplier = "extension_multiplier"
	AttributeKeyMarginMode          = "margin_mode"
	AttributeKeyMinTransferAmount   = "min_transfer_amount"
	AttributeKeyMinWithdrawalAmount = "min_withdrawal_amount"
	AttributeKeyEnabled             = "enabled"
	AttributeKeyMaxAssetIndex       = "max_asset_index"
)
