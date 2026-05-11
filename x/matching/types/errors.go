package types

import "cosmossdk.io/errors"

var (
	ErrInvalidAuthority   = errors.Register(ModuleName, 2, "invalid authority")
	ErrInvalidOrder       = errors.Register(ModuleName, 3, "invalid order")
	ErrUnauthorized       = errors.Register(ModuleName, 4, "unauthorized")
	ErrPostOnlyCross      = errors.Register(ModuleName, 5, "post-only order would cross book")
	ErrSelfTrade          = errors.Register(ModuleName, 6, "self-trade prevented")
	ErrPriceUnreachable   = errors.Register(ModuleName, 7, "price not reachable in current book")
	ErrIOCNoMatch         = errors.Register(ModuleName, 8, "IOC order has no opposite to match")
	ErrTooManyFills       = errors.Register(ModuleName, 9, "max_fills_per_msg reached")
	ErrTooManyCancels     = errors.Register(ModuleName, 10, "max_cancels_per_msg reached")
	ErrInvalidParams      = errors.Register(ModuleName, 11, "invalid params")
	ErrOrderNotFound      = errors.Register(ModuleName, 12, "order not found")
	ErrTriggerInactive    = errors.Register(ModuleName, 13, "trigger order not yet active")
	ErrReduceOnlyViolated = errors.Register(ModuleName, 14, "reduce-only invariant violated")
	ErrPoolCannotPlaceOrder = errors.Register(ModuleName, 15, "public pool / insurance fund cannot place orders directly")
	ErrDuplicateClientOrder = errors.Register(ModuleName, 16, "duplicate client_order_index for open order")
	ErrOrderNotCancelable   = errors.Register(ModuleName, 17, "order status does not allow cancel/modify")
	ErrUnimplemented        = errors.Register(ModuleName, 18, "feature not implemented")
	ErrQuoteLimitExceeded   = errors.Register(ModuleName, 19, "base*price exceeds market quote limit")
	// ErrAccountUnderLiquidation rejects user-initiated CreateOrder /
	// ModifyOrder calls when the account is currently being
	// liquidated (PARTIAL / FULL / BANKRUPTCY) or, in PRE, when the
	// order would not strictly reduce exposure (i.e. is not
	// reduce-only). Mirrors the "no exchange operation that
	// increases position size or worsens TAV/MMR ratio" rule.
	ErrAccountUnderLiquidation = errors.Register(ModuleName, 20, "account under liquidation; only reduce-only orders allowed")
	// ErrTooManyOpenOrders rejects a CreateOrder when the account has
	// already reached the market's MaxOpenOrdersPerAccount cap.
	// Per-market open_order_count bounds prevent adversarial spamming
	// of post-only / non-funded orders.
	ErrTooManyOpenOrders = errors.Register(ModuleName, 21, "account has reached market open order cap")
)
