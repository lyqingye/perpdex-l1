package perp

import (
	"cosmossdk.io/math"

	accounttypes "github.com/perpdex/perpdex-l1/x/account/types"
)

// clonePosition returns a value copy with all math.Int fields
// guaranteed non-nil so downstream arithmetic doesn't blow up on a
// freshly-defaulted record.
func clonePosition(p accounttypes.AccountPosition) accounttypes.AccountPosition {
	out := p
	if out.Position.IsNil() {
		out.Position = math.ZeroInt()
	}
	if out.EntryQuote.IsNil() {
		out.EntryQuote = math.ZeroInt()
	}
	if out.LastFundingRatePrefixSum.IsNil() {
		out.LastFundingRatePrefixSum = math.ZeroInt()
	}
	if out.AllocatedMargin.IsNil() {
		out.AllocatedMargin = math.ZeroInt()
	}
	return out
}

// ceilDivPositive returns ⌈num/den⌉ for non-negative `num` and
// strictly positive `den`. Mirrors lighter `ceil_div_biguint` on the
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

func sameSign(a, b math.Int) bool {
	if a.IsZero() || b.IsZero() {
		return false
	}
	return a.IsNegative() == b.IsNegative()
}
