package types

import "cosmossdk.io/errors"

var (
	ErrOrderNotFound      = errors.Register(ModuleName, 2, "order not found")
	ErrOrderExists        = errors.Register(ModuleName, 3, "order already exists")
	ErrInvalidOrder       = errors.Register(ModuleName, 4, "invalid order")
	ErrInvalidParams      = errors.Register(ModuleName, 5, "invalid params")
	ErrPriceLevelMissing  = errors.Register(ModuleName, 6, "price level missing")
	ErrInvariantViolated  = errors.Register(ModuleName, 7, "orderbook invariant violated")
	ErrInsufficientLiquidity = errors.Register(ModuleName, 8, "insufficient orderbook liquidity")
	ErrQuoteOverflow         = errors.Register(ModuleName, 9, "base*price exceeds MaxOrderQuoteAmount")
	ErrPriceLevelOverflow    = errors.Register(ModuleName, 10, "price level quote sum would overflow uint64")
	ErrOrderNotCancelable    = errors.Register(ModuleName, 11, "order status does not allow cancel")
)
