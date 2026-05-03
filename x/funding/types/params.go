package types

import perptypes "github.com/perpdex/perpdex-l1/types"

func DefaultParams() Params {
	return Params{
		FundingPeriodMs:       perptypes.FundingPeriod,
		FundingPeriodDivisor:  perptypes.FundingPeriodDivisor,
		MaxPremiumSampleCount: perptypes.MaxPremiumSampleCount,
	}
}

func (p Params) Validate() error {
	if p.FundingPeriodMs <= 0 {
		return ErrInvalidParams.Wrap("funding_period_ms must be > 0")
	}
	if p.FundingPeriodDivisor <= 0 {
		return ErrInvalidParams.Wrap("funding_period_divisor must be > 0")
	}
	return nil
}
