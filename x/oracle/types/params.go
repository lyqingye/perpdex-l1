package types

func DefaultParams() Params {
	return Params{
		MaxAgeMs:              60_000,
		MarkPriceEmaAlpha:     2_000,
		MinVotingPowerRatio:   6_667, // 2/3 in bps
		DeviationThresholdBps: 500,
		VoteExtensionEnabled:  true,
	}
}

func (p Params) Validate() error {
	if p.MaxAgeMs <= 0 {
		return ErrInvalidParams.Wrap("max_age_ms must be > 0")
	}
	if p.MarkPriceEmaAlpha > 10_000 {
		return ErrInvalidParams.Wrap("mark_price_ema_alpha must be <= 10000 (bps)")
	}
	if p.MinVotingPowerRatio > 10_000 {
		return ErrInvalidParams.Wrap("min_voting_power_ratio must be <= 10000 (bps)")
	}
	return nil
}
