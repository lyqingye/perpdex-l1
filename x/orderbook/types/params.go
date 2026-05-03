package types

import perptypes "github.com/perpdex/perpdex-l1/types"

func DefaultParams() Params {
	return Params{
		MaxFillsPerMsg:   100,
		MaxCancelsPerMsg: 200,
		ImpactUsdcAmount: perptypes.ImpactUSDCAmount,
	}
}

func (p Params) Validate() error {
	if p.MaxFillsPerMsg == 0 {
		return ErrInvalidParams.Wrap("max_fills_per_msg must be > 0")
	}
	if p.MaxCancelsPerMsg == 0 {
		return ErrInvalidParams.Wrap("max_cancels_per_msg must be > 0")
	}
	if p.ImpactUsdcAmount == 0 {
		return ErrInvalidParams.Wrap("impact_usdc_amount must be > 0")
	}
	return nil
}
