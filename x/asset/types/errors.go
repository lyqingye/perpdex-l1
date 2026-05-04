package types

import "cosmossdk.io/errors"

var (
	ErrInvalidAuthority      = errors.Register(ModuleName, 2, "invalid authority")
	ErrAssetNotFound         = errors.Register(ModuleName, 3, "asset not found")
	ErrAssetExists           = errors.Register(ModuleName, 4, "asset already registered")
	ErrInvalidAssetParams    = errors.Register(ModuleName, 5, "invalid asset parameters")
	ErrUSDCMarginConstraint  = errors.Register(ModuleName, 6, "only USDC may be margin enabled and USDC must be margin enabled")
	ErrAssetIndexExceedsMax  = errors.Register(ModuleName, 7, "asset index exceeds maximum")
	ErrAssetDisabled         = errors.Register(ModuleName, 8, "asset is disabled")
	ErrInvalidParams         = errors.Register(ModuleName, 9, "invalid params")
)
