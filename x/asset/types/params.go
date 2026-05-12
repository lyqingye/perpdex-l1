package types

import perptypes "github.com/perpdex/perpdex-l1/types"

// DefaultParams returns the default asset module params.
func DefaultParams() Params {
	return Params{
		MaxAssetIndex: perptypes.MaxAssetIndex,
	}
}

// Validate sanity-checks Params.
func (p Params) Validate() error {
	if p.MaxAssetIndex == 0 {
		return ErrInvalidModuleParams.Wrap("max_asset_index must be > 0")
	}
	if p.MaxAssetIndex < perptypes.MinAssetIndex {
		return ErrInvalidModuleParams.Wrapf(
			"max_asset_index=%d must be >= MinAssetIndex=%d",
			p.MaxAssetIndex, perptypes.MinAssetIndex,
		)
	}
	if p.MaxAssetIndex > perptypes.MaxAssetIndex {
		return ErrInvalidModuleParams.Wrapf(
			"max_asset_index=%d exceeds protocol cap %d",
			p.MaxAssetIndex, perptypes.MaxAssetIndex,
		)
	}
	return nil
}
