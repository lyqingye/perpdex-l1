package perp

import (
	"cosmossdk.io/math"
)

// ceilDivPositive returns ⌈num/den⌉ for non-negative `num` and
// strictly positive `den`. Mirrors `ceil_div_biguint` on the
// non-negative branch (the negative-numerator branch is handled in
// `calculateIsolatedMarginDelta` via the `oldMV <= 0` short-circuit).
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
