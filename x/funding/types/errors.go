package types

import "cosmossdk.io/errors"

var (
	ErrInvalidParams = errors.Register(ModuleName, 2, "invalid params")
)
