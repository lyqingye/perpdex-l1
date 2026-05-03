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
)
