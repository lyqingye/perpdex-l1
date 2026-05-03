package types

import perptypes "github.com/perpdex/perpdex-l1/types"

func DefaultParams() Params {
	return Params{
		AggregationMode:        perptypes.OracleAggWhitelist,
		MaxAgeMs:               60_000,
		MarkPriceEmaAlpha:      2_000,
		MinVotingPowerRatio:    6_667, // 2/3 in bps
		DeviationThresholdBps:  500,
		SlashFractionDeviation: "0.001",
		SlashFractionDowntime:  "0.0001",
		MinActiveRatioBps:      8_000,
		ActiveWindowBlocks:     1_000,
		MaxConsecutiveMissed:   100,
		VoteExtensionEnabled:   false,
	}
}

func (p Params) Validate() error {
	if p.AggregationMode != perptypes.OracleAggPosMedian && p.AggregationMode != perptypes.OracleAggWhitelist {
		return ErrInvalidParams.Wrap("aggregation_mode out of range")
	}
	if p.MaxAgeMs <= 0 {
		return ErrInvalidParams.Wrap("max_age_ms must be > 0")
	}
	return nil
}
