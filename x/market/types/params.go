package types

import perptypes "github.com/perpdex/perpdex-l1/types"

func DefaultParams() Params {
	return Params{
		MaxPerpsMarketIndex: perptypes.MaxPerpsMarketIndex,
		MinSpotMarketIndex:  perptypes.MinSpotMarketIndex,
		MaxSpotMarketIndex:  perptypes.MaxSpotMarketIndex,
	}
}

func (p Params) Validate() error {
	if p.MinSpotMarketIndex == 0 || p.MaxSpotMarketIndex == 0 {
		return ErrInvalidParams.Wrap("spot index range must be > 0")
	}
	if p.MaxPerpsMarketIndex >= p.MinSpotMarketIndex {
		return ErrInvalidParams.Wrap("perps index range overlaps spot range")
	}
	return nil
}
