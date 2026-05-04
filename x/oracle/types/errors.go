package types

import "cosmossdk.io/errors"

var (
	ErrInvalidAuthority   = errors.Register(ModuleName, 2, "invalid authority")
	ErrPriceNotFound      = errors.Register(ModuleName, 3, "oracle price not found")
	ErrProviderNotFound   = errors.Register(ModuleName, 4, "oracle provider not found")
	ErrProviderDisabled   = errors.Register(ModuleName, 5, "oracle provider disabled")
	ErrBindingNotFound    = errors.Register(ModuleName, 6, "validator oracle binding not found")
	ErrBindingExists      = errors.Register(ModuleName, 7, "validator oracle binding already exists")
	ErrInvalidMode        = errors.Register(ModuleName, 8, "invalid aggregation mode")
	ErrInvalidParams      = errors.Register(ModuleName, 9, "invalid params")
	ErrUnauthorized       = errors.Register(ModuleName, 10, "unauthorized signer")
	ErrInvalidPrice       = errors.Register(ModuleName, 11, "invalid price")
	ErrInvalidVote        = errors.Register(ModuleName, 12, "invalid oracle vote")
	ErrStalePrice         = errors.Register(ModuleName, 13, "oracle price is stale")
	ErrVoteExtDisabled    = errors.Register(ModuleName, 14, "vote-extension aggregation not implemented")
)
