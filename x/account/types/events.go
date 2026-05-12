package types

// The account module emits events on TWO complementary layers:
//
//  1. State-change events (typed, declared in events.proto and generated
//     into events.pb.go):
//       - EventAccountUpdated
//       - EventAccountAssetUpdated
//       - EventPositionUpdated
//     These are emitted inside the keeper write primitives
//     (createAccount / updateAccount / setAccountAsset / setPosition)
//     so EVERY persisted mutation produces an event, no matter which
//     caller drove it (x/trade fills, x/funding settlement,
//     x/liquidation, x/orderbook lock / release, etc.). Each event
//     carries the full post-write row snapshot, so an off-chain
//     indexer that consumes ONLY these three events can rebuild the
//     canonical Accounts / AccountAssets / AccountPositions tables
//     with last-write-wins semantics.
//
//  2. Business events (string-attribute, declared below): emitted in
//     the x/account msg_server handlers (Deposit, Withdraw, Transfer,
//     MintShares, …) so dashboards / wallets can show user-facing
//     activity with caller intent (route type, target asset, amount).
//     These events DO NOT enumerate every state mutation — for
//     example a trade fill mutates positions / balances without ever
//     touching the msg_server. State reconstruction MUST rely on the
//     typed events in (1); business events are a UX convenience layer.

// Event types for the account module (business / intent layer).
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
