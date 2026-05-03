package types

import "cosmossdk.io/errors"

var (
	// ErrMissingPrice indicates an attempt to compute risk for a
	// non-zero position whose oracle price could not be resolved. The
	// previous implementation silently skipped such positions, which
	// let a regression where the oracle becomes unavailable make
	// every account look HEALTHY.
	ErrMissingPrice = errors.Register(ModuleName, 2, "oracle price missing for position")

	// ErrStalePrice fires when an oracle price is older than
	// `oracle.Params.MaxAgeMs`. Used by risk / liquidation / funding to
	// refuse acting on stale data.
	ErrStalePrice = errors.Register(ModuleName, 3, "oracle price is stale")

	// ErrZeroMarkPrice fires when the mark price is zero while the
	// account holds a non-zero position. Treated as staleness since a
	// zero mark can never be a legitimate live value.
	ErrZeroMarkPrice = errors.Register(ModuleName, 4, "oracle mark price is zero")
)
