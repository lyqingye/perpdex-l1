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
		return ErrInvalidAssetParams.Wrap("max_asset_index must be > 0")
	}
	return nil
}
