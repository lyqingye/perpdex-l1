package types

const (
	// UPerpDenom is the base (smallest) denom of the chain.
	UPerpDenom = "uperp"
	// HumanCoinUnit is the human readable name of the chain's display denom.
	HumanCoinUnit = "perp"
	// BaseCoinUnit is the base denom (alias for UPerpDenom).
	BaseCoinUnit = "uperp"
	// PerpExponent is the exponent of the human readable denom.
	PerpExponent = 6
)

// Time constants (milliseconds).
const (
	SecondInMs = int64(1_000)
	MinuteInMs = int64(60_000)
	HourInMs   = int64(3_600_000)
)

// Funding constants. See zk-dex/l1/02-data-types.md §2.3.
const (
	FundingPeriod         = HourInMs
	FundingPeriodDivisor  = int64(8)
	FundingSmallClamp     = int64(500)
	FundingBigClamp       = int64(40_000)
	MaxPremiumSampleCount = uint32(60)
	FundingRateTick       = int64(1_000_000)
)

// Mark-price constants.
const (
	// MarkPremiumEmaTauMs is the EMA time constant used by
	// refreshMarkPrice to smooth `clamp(impact - index, ±index/200)`
	// into the price_1 premium component (8 minutes).
	MarkPremiumEmaTauMs = int64(8 * 60 * 1_000)
	// MarkPremiumClampDivisor caps |premium| at `index / divisor`
	// (i.e. ±0.5% of index for divisor=200) so a single impact-price
	// spike cannot drag mark price arbitrarily far from index.
	MarkPremiumClampDivisor = int64(200)
)

// Precision / tick constants. See zk-dex/l1/02-data-types.md §2.2.
const (
	OneUSDC                    = uint64(1_000_000)
	USDCToCollateralMultiplier = uint64(1_000_000)
	OneUSDCCollateral          = uint64(1_000_000_000_000)
	ImpactUSDCAmount           = uint64(500_000_000)
	FeeTick                    = uint64(1_000_000)
	MarginTick                 = uint32(10_000)
	MarginFractionMultiplier   = uint64(100) // USDC_TO_COLLATERAL_MULTIPLIER / MARGIN_TICK
	ShareTick                  = uint32(10_000)
)

// Account / market index ranges. See zk-dex/l1/02-data-types.md §2.4.
const (
	MaxAccountIndex                 = uint64(281_474_976_710_654)
	NilAccountIndex                 = uint64(281_474_976_710_655)
	MaxMasterAccountIndex           = uint64(140_737_488_355_327)
	MinSubAccountIndex              = uint64(140_737_488_355_328)
	NilMasterAccountIndex           = uint64(0)
	TreasuryAccountIndex            = uint64(0)
	InsuranceFundOperatorAccountIdx = uint64(1)
	MaxPerpsMarketIndex             = uint32(254)
	MinSpotMarketIndex              = uint32(2048)
	MaxSpotMarketIndex              = uint32(4094)
	NilMarketIndex                  = uint32(255)
	PositionListSize                = uint32(255)
	FirstUserMasterAccountIndex     = uint64(2)
)

// Asset constants.
const (
	NativeAssetIndex = uint32(1)
	LITAssetIndex    = uint32(2)
	USDCAssetIndex   = uint32(3)
	MinAssetIndex    = uint32(1)
	MaxAssetIndex    = uint32(62)
	NilAssetIndex    = uint32(0)

	// USDCDenom is the canonical bank denom assigned to the
	// genesis-seeded USDC margin asset. Reserved from runtime
	// MsgRegisterAsset use; only the genesis seed may bind it.
	USDCDenom = "uusdc"
	// USDCDisplayName is the canonical human label for USDC. Reserved
	// from runtime use; comparisons are case-insensitive (assets.go
	// trims whitespace + folds case before comparing).
	USDCDisplayName = "USDC"

	// MaxAssetDecimals caps the on-chain `decimals` field. 18 covers
	// every EVM-era token (WETH, DAI, ...) without exposing the chain
	// to silly values like 255 that downstream math would have to
	// special-case.
	MaxAssetDecimals = uint32(18)
	// MaxExtensionMultiplier caps `extension_multiplier`. The internal
	// USDC collateral precision is 10^12 (USDCToCollateralMultiplier =
	// 1_000_000), so 10^18 leaves comfortable headroom for assets that
	// extend further while preventing pathologically large multipliers
	// that would let a single deposit produce astronomically large
	// math.Int balances.
	MaxExtensionMultiplier = uint64(1_000_000_000_000_000_000)
	// MaxAssetDisplayNameLen caps display_name. Generous enough for
	// normal labels (e.g. "USDC", "wstETH") but bounded so the field
	// can't grow into a kilobyte memo.
	MaxAssetDisplayNameLen = 32
)

// Order constants.
const (
	MaxOrderPrice       = uint32(4_294_967_295)
	MaxOrderBaseAmount  = uint64(281_474_976_710_655)
	MaxOrderQuoteAmount = uint64(281_474_976_710_655)
	FirstAskNonce       = int64(1)
	FirstBidNonce       = int64(281_474_976_710_655)
	MaxNonce            = int64(281_474_976_710_655)
	MaxSkipNonceCap     = int64(140_737_488_355_327)
	MinClientOrderIndex = uint64(1)
	MaxClientOrderIndex = uint64(281_474_976_710_655)
)

// Perp position bit-width bounds. The prover circuit constrains
// `POSITION_SIZE_BITS = 56` and `ENTRY_QUOTE_BITS = 56`; we mirror the
// limits here so trade application can refuse fills that would push
// |position| or |entry_quote| beyond what the prover would accept,
// classifying the failure as a recoverable maker / taker error.
const (
	PositionSizeBits = uint8(56)
	EntryQuoteBits   = uint8(56)
	MaxPositionSize  = uint64(1<<56 - 1)
	MaxEntryQuote    = uint64(1<<56 - 1)
)

// Min transfer / withdraw amounts (USDC 6-decimal external precision).
const (
	MinPartialTransferAmount = uint64(10_000_000)
	MinPartialWithdrawAmount = uint64(10_000_000)
)

// Account types.
const (
	MasterAccountType        = uint32(0)
	SubAccountType           = uint32(1)
	PublicPoolAccountType    = uint32(2)
	InsuranceFundAccountType = uint32(3)
)

// Account trading modes.
const (
	AccountTradingModeSimple  = uint32(0)
	AccountTradingModeUnified = uint32(1)
)

// Public pool constants, mirroring circuit/src/types/constants.rs
// `INITIAL_POOL_SHARE_VALUE`, `NB_STRATEGIES`, `SHARES_LIST_SIZE` and the
// PUBLIC_POOL status enum.
const (
	NbStrategies               = 8
	SharesListSize             = 16
	InitialPoolShareValue      = uint64(1_000) // 0.001 USDC
	PublicPoolStatusActive     = uint32(0)
	PublicPoolStatusFrozen     = uint32(1)
	DefaultLLPCooldownPeriodMs = int64(7 * 24 * 60 * 60 * 1000) // 7 days
)

// Market types & status.
const (
	MarketTypePerps     = uint32(0)
	MarketTypeSpot      = uint32(1)
	MarketStatusExpired = uint32(0)
	MarketStatusActive  = uint32(1)
)

// Asset routing.
const (
	RouteTypePerps = uint32(0)
	RouteTypeSpot  = uint32(1)
)

// Margin modes.
const (
	MarginModeDisabled = uint32(0)
	MarginModeEnabled  = uint32(1)
	CrossMargin        = uint32(0)
	IsolatedMargin     = uint32(1)
	RemoveMargin       = uint32(0)
	AddMargin          = uint32(1)
)

// Order types.
const (
	LimitOrder           = uint32(0)
	MarketOrder          = uint32(1)
	StopLossOrder        = uint32(2)
	StopLossLimitOrder   = uint32(3)
	TakeProfitOrder      = uint32(4)
	TakeProfitLimitOrder = uint32(5)
	TWAPOrder            = uint32(6)
	TWAPSubOrder         = uint32(7)
	LiquidationOrder     = uint32(8)
)

// Time-in-force.
const (
	IOC      = uint32(0)
	GTT      = uint32(1)
	PostOnly = uint32(2)
)

// Trigger statuses.
const (
	TriggerStatusNA          = uint32(0)
	TriggerStatusMarkPrice   = uint32(1)
	TriggerStatusTWAP        = uint32(2)
	TriggerStatusParentOrder = uint32(3)
)

// Order status.
const (
	OrderStatusOpen             = uint32(0)
	OrderStatusPartiallyFilled  = uint32(1)
	OrderStatusFilled           = uint32(2)
	OrderStatusCancelled        = uint32(3)
	OrderStatusTriggeredPending = uint32(4)
)

// Health status.
const (
	HealthHealthy            = uint32(0)
	HealthPreLiquidation     = uint32(1)
	HealthPartialLiquidation = uint32(2)
	HealthFullLiquidation    = uint32(3)
	HealthBankruptcy         = uint32(4)
)

// Cancel-all modes.
const (
	ImmediateCancelAll      = uint32(0)
	ScheduledCancelAll      = uint32(1)
	AbortScheduledCancelAll = uint32(2)
)

// Oracle aggregation method tag stored on every OraclePrice record. The
// chain currently runs a single dydx/Slinky-style PoS weighted-median
// aggregation; the constant is kept around so downstream consumers can
// branch on it if alternative aggregations are added later.
const (
	OracleAggPosMedian = uint32(0)
)

const (
	// Bech32PrefixAccAddr defines the Bech32 prefix of an account's address.
	Bech32PrefixAccAddr = "px"
	// Bech32PrefixAccPub defines the Bech32 prefix of an account's public key.
	Bech32PrefixAccPub = "pxpub"
	// Bech32PrefixValAddr defines the Bech32 prefix of a validator's operator address.
	Bech32PrefixValAddr = "pxvaloper"
	// Bech32PrefixValPub defines the Bech32 prefix of a validator's operator public key.
	Bech32PrefixValPub = "pxvaloperpub"
	// Bech32PrefixConsAddr defines the Bech32 prefix of a consensus node address.
	Bech32PrefixConsAddr = "pxvalcons"
	// Bech32PrefixConsPub defines the Bech32 prefix of a consensus node public key.
	Bech32PrefixConsPub = "pxvalconspub"
)

// CoinType is the BIP-44 coin type used by the chain.
const CoinType = uint32(118)
