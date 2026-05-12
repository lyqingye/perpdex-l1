package types

import (
	"cosmossdk.io/math"

	perptypes "github.com/perpdex/perpdex-l1/types"
)

func (d *MarketDetails) NormalizeIntFields() {
	if d.FundingRatePrefixSum.IsNil() {
		d.FundingRatePrefixSum = math.ZeroInt()
	}
}

// InitialMargin returns IM = posAbs * markPrice * DefaultInitialMarginFraction
// / MarginTick. `posAbs` MUST be non-negative.
func (d MarketDetails) InitialMargin(posAbs math.Int, markPrice uint32) math.Int {
	if posAbs.IsNil() || posAbs.IsZero() || markPrice == 0 || d.DefaultInitialMarginFraction == 0 {
		return math.ZeroInt()
	}
	notional := posAbs.Mul(math.NewIntFromUint64(uint64(markPrice)))
	return notional.Mul(math.NewIntFromUint64(uint64(d.DefaultInitialMarginFraction))).
		Quo(math.NewInt(int64(perptypes.MarginTick)))
}
