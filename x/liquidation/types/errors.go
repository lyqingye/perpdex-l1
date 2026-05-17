package types

import "cosmossdk.io/errors"

var (
	ErrNotLiquidatable        = errors.Register(ModuleName, 2, "account is not liquidatable")
	ErrNotBankrupt            = errors.Register(ModuleName, 3, "account is not in bankruptcy")
	ErrInvalidParams          = errors.Register(ModuleName, 4, "invalid params")
	ErrUnauthorized           = errors.Register(ModuleName, 5, "unauthorized")
	ErrInsuranceUnderfunded   = errors.Register(ModuleName, 6, "insurance fund underfunded")
	ErrInvalidADLCounterparty = errors.Register(ModuleName, 7, "invalid ADL counterparty")
	// ErrInsufficientCollateral is reserved for a deleverager-side
	// "has enough cross collateral" guard. No production code path
	// currently raises it: the user-ADL pre-trade collateral assert
	// was removed by F6, and the post-fill HEALTHY check in
	// `Deleverage` returns `ErrInvalidADLCounterparty` instead. The
	// constant is kept registered (removing it would re-number
	// downstream codes in the cosmossdk error registry and break
	// historical event indexers) and `tryLLPAbsorb` keeps a
	// defensive `errors.Is` catch that now hard-fails on this
	// sentinel — see `x/liquidation/keeper/llp.go` for the
	// invariant violation rationale.
	ErrInsufficientCollateral = errors.Register(ModuleName, 8, "insufficient collateral for deleverage")
)
