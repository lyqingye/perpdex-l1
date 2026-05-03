package types

import "cosmossdk.io/errors"

var (
	ErrInvalidAuthority    = errors.Register(ModuleName, 2, "invalid authority")
	ErrAccountNotFound     = errors.Register(ModuleName, 3, "account not found")
	ErrAccountExists       = errors.Register(ModuleName, 4, "account already exists")
	ErrUnauthorized        = errors.Register(ModuleName, 5, "unauthorized")
	ErrInvalidParams       = errors.Register(ModuleName, 6, "invalid params")
	ErrInsufficientFunds   = errors.Register(ModuleName, 7, "insufficient funds")
	ErrInvalidRoute        = errors.Register(ModuleName, 8, "invalid route type")
	ErrAssetDisabled       = errors.Register(ModuleName, 9, "asset disabled")
	ErrAssetNotMargin      = errors.Register(ModuleName, 10, "asset cannot be used as perp collateral")
	ErrAmountTooSmall      = errors.Register(ModuleName, 11, "amount below module minimum")
	ErrInvalidAccountType  = errors.Register(ModuleName, 12, "invalid account type for operation")
	ErrInvalidMarginAction = errors.Register(ModuleName, 13, "invalid margin action")
	ErrPositionNotIsolated = errors.Register(ModuleName, 14, "position is not isolated")
	ErrPositionNotEmpty    = errors.Register(ModuleName, 15, "position must be empty for this operation")
	ErrAccountIndexExceed  = errors.Register(ModuleName, 16, "account index exceeded maximum")
	ErrRiskRegression      = errors.Register(ModuleName, 17, "risk would regress")
	ErrInvalidTradingMode  = errors.Register(ModuleName, 18, "invalid trading mode")
)
