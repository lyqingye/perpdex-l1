package types

import (
	"cosmossdk.io/math"
)

// NormalizeIntFields rewrites every nil math.Int on the row to
// math.ZeroInt() so callers can do prefix-sum arithmetic without
// re-checking IsNil. The keeper's GetMarketDetails funnel-point
// invokes this so consumers (funding settlement, query helpers)
// don't have to repeat the guard.
func (d *MarketDetails) NormalizeIntFields() {
	if d.FundingRatePrefixSum.IsNil() {
		d.FundingRatePrefixSum = math.ZeroInt()
	}
}
