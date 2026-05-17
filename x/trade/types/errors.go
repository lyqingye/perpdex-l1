package types

import "cosmossdk.io/errors"

// Sentinels for x/trade. "Soft" sentinels (Maker*/Taker*) let the
// matching loop evict the offending side and continue; ErrInvalid*
// are hard invariant violations that must revert the whole tx.
var (
	// Soft: maker fails post-trade IsValidRiskChangeFrom.
	ErrMakerRiskRegression = errors.Register(ModuleName, 2, "maker post-trade risk regression")

	// Soft: maker spot balance shortfall (locked or available).
	// Defensive — should be rare with lock-on-place.
	ErrMakerInsufficientBalance = errors.Register(ModuleName, 3, "maker insufficient balance for fill")

	// Soft: taker fails post-trade IsValidRiskChangeFrom. Prior fills
	// in this taker are preserved via writeCache.
	ErrTakerRiskRegression = errors.Register(ModuleName, 4, "taker post-trade risk regression")

	// Soft: taker spot balance shortfall. Prior fills preserved.
	ErrTakerInsufficientBalance = errors.Register(ModuleName, 5, "taker insufficient balance for fill")

	// Soft: maker post-trade |position| or |entry_quote| overflow.
	ErrMakerInvalidPosition = errors.Register(ModuleName, 6, "maker post-trade position out of bounds")

	// Soft: taker post-trade |position| or |entry_quote| overflow.
	ErrTakerInvalidPosition = errors.Register(ModuleName, 7, "taker post-trade position out of bounds")

	// Soft: maker isolated margin_delta exceeds available cross USDC.
	ErrMakerInsufficientCollateral = errors.Register(ModuleName, 8, "maker insufficient cross collateral for isolated margin allocation")

	// Soft: taker isolated margin_delta exceeds available cross USDC.
	ErrTakerInsufficientCollateral = errors.Register(ModuleName, 9, "taker insufficient cross collateral for isolated margin allocation")

	// Hard: a spot debit received a negative amount; upstream invariant
	// violated and the tx must revert.
	ErrInvalidTransferAmount = errors.Register(ModuleName, 10, "transfer amount must be non-negative")
)

// IsRecoverableMakerError reports whether err lets the matching loop
// evict this maker and try the next resting order.
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

// IsRecoverableTakerError reports whether err lets the matching loop
// stop the taker while preserving the fills already written.
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
