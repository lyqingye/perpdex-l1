package keeper

import (
	"context"
	"fmt"
	"sort"

	"cosmossdk.io/math"

	markettypes "github.com/perpdex/perpdex-l1/x/market/types"
)

// clampInt64 clamps v into [lo, hi]. Caller guarantees lo <= hi.
func clampInt64(v, lo, hi int64) int64 {
	if v < lo {
		return lo
	}
	if v > hi {
		return hi
	}
	return v
}

// clampUint32 clamps `v` into the uint32 domain.
func clampUint32(v int64) uint32 {
	if v < 0 {
		return 0
	}
	const maxU32 = int64(1<<32 - 1)
	if v > maxU32 {
		return uint32(maxU32)
	}
	return uint32(v)
}

// clampInt clamps a math.Int into [lo, hi]. Caller guarantees lo.LTE(hi).
func clampInt(v, lo, hi math.Int) math.Int {
	if v.LT(lo) {
		return lo
	}
	if v.GT(hi) {
		return hi
	}
	return v
}

// median3Uint32 returns the median of three uint32 inputs.
func median3Uint32(a, b, c uint32) uint32 {
	xs := [3]uint32{a, b, c}
	sort.Slice(xs[:], func(i, j int) bool { return xs[i] < xs[j] })
	return xs[1]
}

// mustSetMarketDetails persists the runtime market details and panics on
// failure. A store write failure is state-machine corruption (out-of-disk
// or store bug); continuing with stale in-memory state is unsafe.
func (k Keeper) mustSetMarketDetails(ctx context.Context, d markettypes.MarketDetails) {
	if err := k.marketKeeper.SetMarketDetails(ctx, d); err != nil {
		panic(fmt.Errorf("funding: persist market %d details: %w", d.MarketIndex, err))
	}
}
