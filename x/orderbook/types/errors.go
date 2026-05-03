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
)
