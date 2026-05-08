package types

import (
	"cosmossdk.io/math"
)

// NormalizeIntFields rewrites every nil math.Int on the row to math.ZeroInt().
func (d *MarketDetails) NormalizeIntFields() {
	if d.FundingRatePrefixSum.IsNil() {
		d.FundingRatePrefixSum = math.ZeroInt()
	}
}
