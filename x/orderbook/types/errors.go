package types

import "cosmossdk.io/errors"

var (
	ErrOrderNotFound      = errors.Register(ModuleName, 2, "order not found")
	ErrOrderExists        = errors.Register(ModuleName, 3, "order already exists")
	ErrInvariantViolated  = errors.Register(ModuleName, 4, "orderbook invariant violated")
	ErrQuoteOverflow      = errors.Register(ModuleName, 5, "base*price exceeds MaxOrderQuoteAmount")
	ErrPriceLevelOverflow = errors.Register(ModuleName, 6, "price level quote sum would overflow uint64")
	ErrOrderNotCancelable = errors.Register(ModuleName, 7, "order status does not allow cancel")
)
