package types

import "cosmossdk.io/errors"

var (
	ErrNotLiquidatable        = errors.Register(ModuleName, 2, "account is not liquidatable")
	ErrNotBankrupt            = errors.Register(ModuleName, 3, "account is not in bankruptcy")
	ErrInvalidParams          = errors.Register(ModuleName, 4, "invalid params")
	ErrUnauthorized           = errors.Register(ModuleName, 5, "unauthorized")
	ErrInsuranceUnderfunded   = errors.Register(ModuleName, 6, "insurance fund underfunded")
	ErrInvalidADLCounterparty = errors.Register(ModuleName, 7, "invalid ADL counterparty")
)
