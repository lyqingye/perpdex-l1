package types

import "cosmossdk.io/errors"

// Module error codes. Codes are stable on-chain identifiers; do not
// renumber or reuse retired codes.
var (
	ErrInvalidAuthority     = errors.Register(ModuleName, 2, "invalid authority")
	ErrAssetNotFound        = errors.Register(ModuleName, 3, "asset not found")
	ErrAssetExists          = errors.Register(ModuleName, 4, "asset already registered")
	ErrInvalidAssetParams   = errors.Register(ModuleName, 5, "invalid asset parameters")
	ErrUSDCMarginConstraint = errors.Register(ModuleName, 6, "USDC binding violated: only the genesis-seeded USDC may be margin enabled")
	ErrAssetIndexExceedsMax = errors.Register(ModuleName, 7, "asset index exceeds maximum")
	ErrAssetDisabled        = errors.Register(ModuleName, 8, "asset is disabled")
	// ErrInvalidModuleParams flags an invalid Params struct (module-wide
	// configuration). ErrInvalidAssetParams flags an invalid Asset row.
	ErrInvalidModuleParams = errors.Register(ModuleName, 9, "invalid module params")
)
