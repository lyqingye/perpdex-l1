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
	// MaxPremiumSampleCount caps the number of per-minute premium
	// samples accumulated within a funding window. A value of 0 would
	// disable the safety cap entirely; require at least 1 so the
	// per-window aggregation cannot grow unbounded under a degraded
	// throttle.
	if p.MaxPremiumSampleCount == 0 {
		return ErrInvalidParams.Wrap("max_premium_sample_count must be > 0")
	}
	// A funding window shorter than the per-minute sample interval is
	// nonsensical: BeginBlocker would close the round before it could
	// collect any sample.
	if p.FundingPeriodMs < perptypes.MinuteInMs {
		return ErrInvalidParams.Wrapf(
			"funding_period_ms=%d must be >= one premium sample interval (%dms)",
			p.FundingPeriodMs, perptypes.MinuteInMs,
		)
	}
	return nil
}
