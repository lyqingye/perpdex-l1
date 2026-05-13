package types

import perptypes "github.com/perpdex/perpdex-l1/types"

// DefaultMaxMarkStalenessMs caps how stale `MarketDetails.MarkPrice`
// may be (in milliseconds) before downstream consumers (x/risk,
// x/trade, etc.) MUST treat it as missing. The mark price is refreshed
// every block by `BeginBlocker`'s median pipeline, so 5 minutes is a
// generous safety margin that still catches a halted funding loop or
// stalled oracle pipeline.
const DefaultMaxMarkStalenessMs = int64(5 * 60 * 1000)

func DefaultParams() Params {
	return Params{
		FundingPeriodMs:       perptypes.FundingPeriod,
		FundingPeriodDivisor:  perptypes.FundingPeriodDivisor,
		MaxPremiumSampleCount: perptypes.MaxPremiumSampleCount,
		MaxMarkStalenessMs:    DefaultMaxMarkStalenessMs,
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
	// 0 disables the staleness gate (only useful in tests / genesis
	// bootstrapping). Negative is meaningless.
	if p.MaxMarkStalenessMs < 0 {
		return ErrInvalidParams.Wrapf(
			"max_mark_staleness_ms=%d must be >= 0",
			p.MaxMarkStalenessMs,
		)
	}
	return nil
}
