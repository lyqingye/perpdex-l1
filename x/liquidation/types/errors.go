package types

import "cosmossdk.io/errors"

var (
	ErrNotLiquidatable        = errors.Register(ModuleName, 2, "account is not liquidatable")
	ErrNotBankrupt            = errors.Register(ModuleName, 3, "account is not in bankruptcy")
	ErrInvalidParams          = errors.Register(ModuleName, 4, "invalid params")
	ErrUnauthorized           = errors.Register(ModuleName, 5, "unauthorized")
	ErrInsuranceUnderfunded   = errors.Register(ModuleName, 6, "insurance fund underfunded")
	ErrInvalidADLCounterparty = errors.Register(ModuleName, 7, "invalid ADL counterparty")
	// ErrInsufficientCollateral is a reserved sentinel: no production
	// path currently raises it (the user-ADL guard was removed; the
	// post-fill HEALTHY check returns ErrInvalidADLCounterparty
	// instead). Kept registered to preserve the error-code numbering;
	// tryLLPAbsorb treats it as an invariant violation.
	ErrInsufficientCollateral = errors.Register(ModuleName, 8, "insufficient collateral for deleverage")
)
