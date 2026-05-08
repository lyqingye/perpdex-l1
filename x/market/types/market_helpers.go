package types

import (
	"cosmossdk.io/math"

	perptypes "github.com/perpdex/perpdex-l1/types"
)

// NormalizeIntFields rewrites every nil math.Int on the row to math.ZeroInt().
func (d *MarketDetails) NormalizeIntFields() {
	if d.FundingRatePrefixSum.IsNil() {
		d.FundingRatePrefixSum = math.ZeroInt()
	}
}

// InitialMargin returns the initial margin requirement for a HYPOTHETICAL
// |posAbs| at `markPrice`, evaluated against the market's
// `DefaultInitialMarginFraction`.
//
//	IM = |posAbs| * markPrice * DefaultInitialMarginFraction / MarginTick
//
// The formula owner is `MarketDetails` because the multiplier
// (`DefaultInitialMarginFraction`) lives here; size and mark are caller-
// supplied so the helper covers both real positions
// (`AccountPosition.InitialMargin` thin-wraps this) and synthetic sizes
// fed by the trade keeper's isolated-margin delta math (|new|, |OI delta|).
//
// Returns ZeroInt for empty size, zero mark, or zero margin fraction.
// `posAbs` MUST be non-negative — callers feed `Position.Abs()` /
// `oiDelta.Abs()`.
func (d MarketDetails) InitialMargin(posAbs math.Int, markPrice uint32) math.Int {
	if posAbs.IsNil() || posAbs.IsZero() || markPrice == 0 || d.DefaultInitialMarginFraction == 0 {
		return math.ZeroInt()
	}
	notional := posAbs.Mul(math.NewIntFromUint64(uint64(markPrice)))
	return notional.Mul(math.NewIntFromUint64(uint64(d.DefaultInitialMarginFraction))).
		Quo(math.NewInt(int64(perptypes.MarginTick)))
}
