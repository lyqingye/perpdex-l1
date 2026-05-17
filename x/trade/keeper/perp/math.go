package perp

import (
	"cosmossdk.io/math"
)

// ceilDivPositive returns ⌈num/den⌉ for non-negative num and positive
// den. The negative-numerator branch is handled by the oldMV <= 0
// short-circuit in calculateIsolatedMarginDelta.
func ceilDivPositive(num, den math.Int) math.Int {
	if den.IsZero() {
		return math.ZeroInt()
	}
	q := num.Quo(den)
	r := num.Mod(den)
	if r.IsZero() {
		return q
	}
	return q.Add(math.OneInt())
}
