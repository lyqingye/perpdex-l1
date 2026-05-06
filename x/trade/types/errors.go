package types

import "cosmossdk.io/errors"

// Sentinel errors for x/trade. These are split into "soft" / recoverable
// errors that the matching engine can use to skip a specific maker (or
// abort just the current taker, preserving prior fills) and "hard" errors
// that must revert the entire transaction.
//
// Lighter parity: `is_valid_perps_trade` / `is_valid_spot_trade` set
// `cancel_taker_order` / `cancel_maker_order` rather than reverting the
// whole block. We mirror that with sentinels that the matching loop
// inspects via errors.Is.
var (
	// ErrMakerRiskRegression: maker side fails IsValidRiskChange after
	// the would-be fill (e.g. maker drained collateral after resting).
	// Soft: matchOrder evicts the maker and continues with the next
	// price level.
	ErrMakerRiskRegression = errors.Register(ModuleName, 2, "maker post-trade risk regression")

	// ErrMakerInsufficientBalance: maker side cannot satisfy a spot
	// transfer (locked or available balance shortfall). Soft: evict
	// maker and continue. Should be rare once spot lock-on-place is
	// enforced, but kept as a defensive fallback.
	ErrMakerInsufficientBalance = errors.Register(ModuleName, 3, "maker insufficient balance for fill")

	// ErrTakerRiskRegression: taker side fails IsValidRiskChange.
	// Soft: matchOrder stops the taker (already-applied fills via
	// writeCache are preserved); remaining base is cancelled.
	ErrTakerRiskRegression = errors.Register(ModuleName, 4, "taker post-trade risk regression")

	// ErrTakerInsufficientBalance: taker spot transfer fails. Soft:
	// stop taker, preserve prior fills.
	ErrTakerInsufficientBalance = errors.Register(ModuleName, 5, "taker insufficient balance for fill")
)

// IsRecoverableMakerError reports whether err is a sentinel that the
// matching loop should treat as "evict this maker and try the next
// resting order".
func IsRecoverableMakerError(err error) bool {
	if err == nil {
		return false
	}
	return errors.IsOf(err, ErrMakerRiskRegression, ErrMakerInsufficientBalance)
}

// IsRecoverableTakerError reports whether err is a sentinel that the
// matching loop should treat as "stop this taker but preserve the
// fills that already wrote through".
func IsRecoverableTakerError(err error) bool {
	if err == nil {
		return false
	}
	return errors.IsOf(err, ErrTakerRiskRegression, ErrTakerInsufficientBalance)
}
