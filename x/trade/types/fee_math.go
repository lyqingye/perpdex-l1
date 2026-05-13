package types

import (
	"cosmossdk.io/math"

	perptypes "github.com/perpdex/perpdex-l1/types"
)

// FeeOf returns the integer fee charged for `notional` quote at the
// supplied bps rate, using the fixed fee tick:
//
//	fee = notional * bps / FeeTick
//
// Truncates toward zero (Quo on cosmos-sdk math.Int does Euclidean
// division). Returns ZeroInt when bps == 0 to avoid an unnecessary
// big.Int multiplication.
func FeeOf(notional math.Int, bps uint32) math.Int {
	if bps == 0 || notional.IsNil() || notional.IsZero() {
		return math.ZeroInt()
	}
	return notional.Mul(math.NewIntFromUint64(uint64(bps))).
		Quo(math.NewInt(int64(perptypes.FeeTick)))
}
