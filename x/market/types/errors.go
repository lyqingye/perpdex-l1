package types

import "cosmossdk.io/errors"

var (
	ErrInvalidAuthority   = errors.Register(ModuleName, 2, "invalid authority")
	ErrMarketNotFound     = errors.Register(ModuleName, 3, "market not found")
	ErrMarketExists       = errors.Register(ModuleName, 4, "market already exists")
	ErrInvalidMarket      = errors.Register(ModuleName, 5, "invalid market")
	ErrInvalidParams      = errors.Register(ModuleName, 6, "invalid params")
	ErrMarketIndexExceed  = errors.Register(ModuleName, 7, "market index out of allowed range")
	ErrMarketNotActive    = errors.Register(ModuleName, 8, "market not active")
	ErrInvalidMarginChain = errors.Register(ModuleName, 9, "invalid margin parameter chain")
	ErrNonceExhausted     = errors.Register(ModuleName, 10, "nonce range exhausted (ask>=bid)")
	ErrOpenInterestLimit  = errors.Register(ModuleName, 11, "open interest exceeds limit")
	ErrZeroMarkPrice      = errors.Register(ModuleName, 12, "mark price is zero")
	ErrStaleMarkPrice     = errors.Register(ModuleName, 13, "mark price is stale")
	ErrMissingPrice       = errors.Register(ModuleName, 14, "mark price unavailable")
)
