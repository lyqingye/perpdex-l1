package types

import "cosmossdk.io/errors"

// Error code numbers must remain stable across releases so external
// indexers can keep matching against the registered sentinels. Codes
// 4, 5, 6 and 8 are intentionally skipped — they were previously
// assigned to ErrInvalidOrder / ErrInvalidParams /
// ErrPriceLevelMissing / ErrInsufficientLiquidity, none of which were
// ever raised from inside x/orderbook (matching / liquidation /
// account have their own equivalents), so the placeholders are kept
// retired rather than reused.
var (
	ErrOrderNotFound      = errors.Register(ModuleName, 2, "order not found")
	ErrOrderExists        = errors.Register(ModuleName, 3, "order already exists")
	ErrInvariantViolated  = errors.Register(ModuleName, 7, "orderbook invariant violated")
	ErrQuoteOverflow      = errors.Register(ModuleName, 9, "base*price exceeds MaxOrderQuoteAmount")
	ErrPriceLevelOverflow = errors.Register(ModuleName, 10, "price level quote sum would overflow uint64")
	ErrOrderNotCancelable = errors.Register(ModuleName, 11, "order status does not allow cancel")
)
