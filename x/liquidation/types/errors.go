package types

import "cosmossdk.io/errors"

var (
	ErrNotLiquidatable        = errors.Register(ModuleName, 2, "account is not liquidatable")
	ErrNotBankrupt            = errors.Register(ModuleName, 3, "account is not in bankruptcy")
	ErrInvalidParams          = errors.Register(ModuleName, 4, "invalid params")
	ErrUnauthorized           = errors.Register(ModuleName, 5, "unauthorized")
	ErrInsuranceUnderfunded   = errors.Register(ModuleName, 6, "insurance fund underfunded")
	ErrInvalidADLCounterparty = errors.Register(ModuleName, 7, "invalid ADL counterparty")
	// ErrInsufficientCollateral is returned by Deleverage / autoADL
	// when the user-ADL deleverager cannot cover the predicted
	// realised PnL with available cross / allocated collateral.
	// Implements the deleverager-side "has enough cross collateral"
	// guard. The bankrupt-side counterpart is intentionally NOT
	// enforced in perpdex — see `liquidate.go` Deleverage docstring
	// for the rationale. EndBlocker callers treat this as a
	// graceful "skip this candidate" signal; user MsgDeleverage
	// callers surface it directly.
	ErrInsufficientCollateral = errors.Register(ModuleName, 8, "insufficient collateral for deleverage")
)
