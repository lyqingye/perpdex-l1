package keeper

import (
	"sort"
)

// WeightedMedian returns the weighted median of (value, weight) samples.
// Implements the algorithm from 19-oracle.md §5: sort by value, walk
// cumulative weight, return the first value whose cumulative weight crosses
// half of total. Returns 0 when input is empty.
func WeightedMedian(values []uint32, weights []uint64) uint32 {
	n := len(values)
	if n == 0 || n != len(weights) {
		return 0
	}
	type pair struct {
		v uint32
		w uint64
	}
	pairs := make([]pair, n)
	var total uint64
	for i := 0; i < n; i++ {
		pairs[i] = pair{values[i], weights[i]}
		total += weights[i]
	}
	sort.Slice(pairs, func(i, j int) bool { return pairs[i].v < pairs[j].v })
	half := total / 2
	var cum uint64
	for _, p := range pairs {
		cum += p.w
		if cum*2 >= total*1 && cum > half {
			return p.v
		}
	}
	return pairs[n-1].v
}

// AbsDiff returns |a-b| for uint32 inputs.
func AbsDiff(a, b uint32) uint32 {
	if a > b {
		return a - b
	}
	return b - a
}
