package perp

import (
	"cosmossdk.io/math"
)

// ceilDivPositive returns ⌈num/den⌉ for non-negative `num` and
// strictly positive `den`. Mirrors `ceil_div_biguint` on the
// non-negative branch (the negative-numerator branch is handled in
// `calculateIsolatedMarginDelta` via the `oldMV <= 0` short-circuit).
//
// `clonePosition` and `sameSign` formerly lived here; both were
// retired during the position-math cohesion pass:
//
//   - GetPosition / IterateAccountPositions in x/account/keeper now
//     normalise math.Int fields at every read entry, so a defensive
//     clone-with-nil-guard is redundant.
//   - sameSign was a duplicate of x/risk/keeper sameSignInt; the pair
//     is now expressed by accounttypes.IsSameSide so trade and risk
//     share one definition.
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
