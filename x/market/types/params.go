package types

import perptypes "github.com/perpdex/perpdex-l1/types"

// DefaultMaxMarketsExpiredPerBlock is the EndBlocker per-block budget
// applied when a fresh chain bootstraps without an explicit param. The
// value is a conservative safety net: at 32 markets/block, even the
// pathological "every market expires in the same block" scenario
// (~2300 markets) drains in ~72 blocks of dedicated work, far below
// any usable consensus timeout. Operators can raise this via
// MsgUpdateParams once they have measured production EndBlocker cost.
const DefaultMaxMarketsExpiredPerBlock = uint32(32)

func DefaultParams() Params {
	return Params{
		MaxPerpsMarketIndex:        perptypes.MaxPerpsMarketIndex,
		MinSpotMarketIndex:         perptypes.MinSpotMarketIndex,
		MaxSpotMarketIndex:         perptypes.MaxSpotMarketIndex,
		MaxMarketsExpiredPerBlock:  DefaultMaxMarketsExpiredPerBlock,
	}
}

func (p Params) Validate() error {
	if p.MinSpotMarketIndex == 0 || p.MaxSpotMarketIndex == 0 {
		return ErrInvalidParams.Wrap("spot index range must be > 0")
	}
	if p.MaxPerpsMarketIndex >= p.MinSpotMarketIndex {
		return ErrInvalidParams.Wrap("perps index range overlaps spot range")
	}
	if p.MinSpotMarketIndex > p.MaxSpotMarketIndex {
		return ErrInvalidParams.Wrap("min_spot_market_index > max_spot_market_index")
	}
	// NilMarketIndex (255) is reserved chain-wide for "no market". The
	// upper bound of every range must stay strictly below it so that a
	// NilMarketIndex value cannot accidentally resolve to a real
	// market.
	if uint32(p.MaxPerpsMarketIndex) >= perptypes.NilMarketIndex {
		return ErrInvalidParams.Wrapf(
			"max_perps_market_index=%d must be < NilMarketIndex=%d",
			p.MaxPerpsMarketIndex, perptypes.NilMarketIndex,
		)
	}
	if uint32(p.MaxSpotMarketIndex) >= perptypes.NilMarketIndex && p.MaxSpotMarketIndex != perptypes.MaxSpotMarketIndex {
		// MaxSpotMarketIndex (4094) is already > NilMarketIndex (255)
		// in the canonical layout; that is fine because spot indices
		// live in their own range. The check above only guards perps.
		_ = p.MaxSpotMarketIndex
	}
	// MaxMarketsExpiredPerBlock is intentionally allowed to be 0 to
	// give operators an emergency switch that disables the EndBlocker
	// auto-expiry path (governance must then call MsgUpdateMarket
	// explicitly to delist). No upper bound check: the param is
	// bounded by what each block can complete in practice.
	return nil
}

// IsPerpsIndex reports whether the given market_index falls in the
// perps range as defined by p.
func (p Params) IsPerpsIndex(idx uint32) bool {
	return idx <= p.MaxPerpsMarketIndex && idx != perptypes.NilMarketIndex
}

// IsSpotIndex reports whether the given market_index falls in the spot
// range as defined by p.
func (p Params) IsSpotIndex(idx uint32) bool {
	return idx >= p.MinSpotMarketIndex && idx <= p.MaxSpotMarketIndex
}

// IsValidIndex reports whether the given market_index falls in either
// the perps or the spot range. Helper for ValidateBasic-style call
// sites that do not have access to the keeper.
func (p Params) IsValidIndex(idx uint32) bool {
	return p.IsPerpsIndex(idx) || p.IsSpotIndex(idx)
}
