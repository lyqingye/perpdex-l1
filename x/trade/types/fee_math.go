package types

import (
	"cosmossdk.io/math"

	perptypes "github.com/perpdex/perpdex-l1/types"
)

// FeeOf returns notional * bps / FeeTick, truncated toward zero.
// Short-circuits on bps == 0 to skip the big.Int multiplication.
func FeeOf(notional math.Int, bps uint32) math.Int {
	if bps == 0 || notional.IsNil() || notional.IsZero() {
		return math.ZeroInt()
	}
	return notional.Mul(math.NewIntFromUint64(uint64(bps))).
		Quo(math.NewInt(int64(perptypes.FeeTick)))
}
