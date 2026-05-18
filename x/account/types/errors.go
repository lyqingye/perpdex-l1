package types

import "cosmossdk.io/errors"

var (
	ErrInvalidAuthority    = errors.Register(ModuleName, 2, "invalid authority")
	ErrAccountNotFound     = errors.Register(ModuleName, 3, "account not found")
	ErrAccountExists       = errors.Register(ModuleName, 4, "account already exists")
	ErrUnauthorized        = errors.Register(ModuleName, 5, "unauthorized")
	ErrInvalidParams       = errors.Register(ModuleName, 6, "invalid params")
	ErrInsufficientFunds   = errors.Register(ModuleName, 7, "insufficient funds")
	ErrInvalidRoute        = errors.Register(ModuleName, 8, "invalid route type")
	ErrAssetDisabled       = errors.Register(ModuleName, 9, "asset disabled")
	ErrAssetNotMargin      = errors.Register(ModuleName, 10, "asset cannot be used as perp collateral")
	ErrAmountTooSmall      = errors.Register(ModuleName, 11, "amount below module minimum")
	ErrInvalidAccountType  = errors.Register(ModuleName, 12, "invalid account type for operation")
	ErrInvalidMarginAction = errors.Register(ModuleName, 13, "invalid margin action")
	ErrPositionNotIsolated = errors.Register(ModuleName, 14, "position is not isolated")
	ErrPositionNotEmpty    = errors.Register(ModuleName, 15, "position must be empty for this operation")
	ErrAccountIndexExceed  = errors.Register(ModuleName, 16, "account index exceeded maximum")
	ErrRiskRegression      = errors.Register(ModuleName, 17, "risk would regress")
	ErrInvalidTradingMode  = errors.Register(ModuleName, 18, "invalid trading mode")

	// Public pool errors.
	ErrPoolFrozen            = errors.Register(ModuleName, 30, "public pool is frozen")
	ErrPoolNotActive         = errors.Register(ModuleName, 31, "public pool is not active")
	ErrCooldownNotElapsed    = errors.Register(ModuleName, 32, "burn cooldown period has not elapsed")
	ErrOperatorRateViolation = errors.Register(ModuleName, 33, "min operator share rate violation")
	ErrNotInsuranceFund      = errors.Register(ModuleName, 34, "operation restricted to insurance fund pools")
	ErrInvalidStrategyIdx    = errors.Register(ModuleName, 35, "invalid strategy index")
	ErrPoolMustBeEmpty       = errors.Register(ModuleName, 36, "pool must be healthy with no positions or open orders")
	ErrSharesListFull        = errors.Register(ModuleName, 37, "user public pool shares list is full")
	ErrInsufficientShares    = errors.Register(ModuleName, 38, "insufficient public pool shares")
	ErrInvalidPoolUpdate     = errors.Register(ModuleName, 39, "invalid public pool update")
	ErrInvalidPoolAccount    = errors.Register(ModuleName, 40, "account is not a public pool")
	ErrPoolCannotPlaceOrder  = errors.Register(ModuleName, 41, "public pool / insurance fund cannot place orders directly")
	ErrPoolGenericMsg        = errors.Register(ModuleName, 42, "public pool / insurance fund cannot use generic account msg; use share/strategy paths")
	ErrInvalidMarginMode     = errors.Register(ModuleName, 44, "invalid margin mode")
	ErrInvalidDepositorIndex = errors.Register(ModuleName, 45, "invalid depositor index")

	// Position lifecycle errors (issue #91).
	//
	// ErrPositionLifecycleViolation signals a buggy caller of the
	// package-private open / mutate / close primitives or of one of
	// the cohesive public methods (ApplyFill, AdjustAllocatedMargin,
	// ApplyFundingPayment, SetPositionLeverage) that violates the
	// per-method pre/post invariants documented on each method.
	ErrPositionLifecycleViolation = errors.Register(ModuleName, 46, "position lifecycle violation")
	// ErrPositionOutOfBounds signals that the post-fill |BaseSize|
	// or |EntryQuote| would overflow the per-market position bounds
	// (perptypes.MaxPositionSize / perptypes.MaxEntryQuote). Surfaced
	// by Keeper.ApplyFill; the trade engine wraps it into
	// Maker/Taker InvalidPosition for the matching loop.
	ErrPositionOutOfBounds = errors.Register(ModuleName, 47, "post-trade position out of bounds")
)
