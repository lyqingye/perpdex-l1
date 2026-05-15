package types

import "cosmossdk.io/errors"

// Sentinel errors for x/trade. These are split into "soft" /
// recoverable errors that the matching engine can use to skip a
// specific maker (or abort just the current taker, preserving prior
// fills) and "hard" errors that must revert the entire transaction.
//
// Soft per-side rejections cancel only the offending order
// ("cancel taker" / "cancel maker") rather than reverting the whole
// block. The matching loop inspects each sentinel via errors.Is.
var (
	// ErrMakerRiskRegression: maker side fails IsValidRiskChangeFrom
	// after the would-be fill (e.g. maker drained collateral after
	// resting). Soft: matchOrder evicts the maker and continues with
	// the next price level.
	ErrMakerRiskRegression = errors.Register(ModuleName, 2, "maker post-trade risk regression")

	// ErrMakerInsufficientBalance: maker side cannot satisfy a spot
	// transfer (locked or available balance shortfall). Soft: evict
	// maker and continue. Should be rare once spot lock-on-place is
	// enforced, but kept as a defensive fallback.
	ErrMakerInsufficientBalance = errors.Register(ModuleName, 3, "maker insufficient balance for fill")

	// ErrTakerRiskRegression: taker side fails IsValidRiskChangeFrom.
	// Soft: matchOrder stops the taker (already-applied fills via
	// writeCache are preserved); remaining base is cancelled.
	ErrTakerRiskRegression = errors.Register(ModuleName, 4, "taker post-trade risk regression")

	// ErrTakerInsufficientBalance: taker spot transfer fails. Soft:
	// stop taker, preserve prior fills.
	ErrTakerInsufficientBalance = errors.Register(ModuleName, 5, "taker insufficient balance for fill")

	// ErrMakerInvalidPosition: maker post-trade position size or
	// entry_quote would overflow the perp bit-width envelope
	// (POSITION_SIZE_BITS / ENTRY_QUOTE_BITS). Soft: evict maker and
	// continue.
	ErrMakerInvalidPosition = errors.Register(ModuleName, 6, "maker post-trade position out of bounds")

	// ErrTakerInvalidPosition: taker post-trade position size or
	// entry_quote overflow. Soft: stop taker, preserve prior fills.
	ErrTakerInvalidPosition = errors.Register(ModuleName, 7, "taker post-trade position out of bounds")

	// ErrMakerInsufficientCollateral: maker isolated position grows
	// (or flips) and the auto-allocated `margin_delta` exceeds the
	// account's available cross collateral. Soft: evict maker and
	// continue.
	ErrMakerInsufficientCollateral = errors.Register(ModuleName, 8, "maker insufficient cross collateral for isolated margin allocation")

	// ErrTakerInsufficientCollateral: taker side of the isolated
	// margin auto-allocation cannot be funded from cross collateral.
	// Soft: stop taker, preserve prior fills.
	ErrTakerInsufficientCollateral = errors.Register(ModuleName, 9, "taker insufficient cross collateral for isolated margin allocation")

	// ErrInvalidTransferAmount fires when a spot maker/taker debit
	// receives a negative `math.Int` amount. This is a hard error
	// (programmer / invariant violation), not a soft sentinel — it
	// cannot be evicted-and-continue because the underlying ApplySpot
	// invariant has been violated upstream.
	ErrInvalidTransferAmount = errors.Register(ModuleName, 10, "transfer amount must be non-negative")
)

// IsRecoverableMakerError reports whether err is a sentinel that the
// matching loop should treat as "evict this maker and try the next
// resting order".
func IsRecoverableMakerError(err error) bool {
	if err == nil {
		return false
	}
	return errors.IsOf(err,
		ErrMakerRiskRegression,
		ErrMakerInsufficientBalance,
		ErrMakerInvalidPosition,
		ErrMakerInsufficientCollateral,
	)
}

// IsRecoverableTakerError reports whether err is a sentinel that the
// matching loop should treat as "stop this taker but preserve the
// fills that already wrote through".
func IsRecoverableTakerError(err error) bool {
	if err == nil {
		return false
	}
	return errors.IsOf(err,
		ErrTakerRiskRegression,
		ErrTakerInsufficientBalance,
		ErrTakerInvalidPosition,
		ErrTakerInsufficientCollateral,
	)
}
