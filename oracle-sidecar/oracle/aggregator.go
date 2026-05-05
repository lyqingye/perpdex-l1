// Package oracle implements the orchestrator running inside the sidecar
// process. It fans observations out to a set of providers, computes the
// per-pair median across providers, evicts stale samples, and exposes the
// latest snapshot to the gRPC server.
package oracle

import (
	"math/big"
	"sort"
	"time"

	"github.com/perpdex/perpdex-l1/oracle-sidecar/providers/types"
)

// Median returns the median of a non-empty slice of *big.Int values. For an
// even-length slice it returns the lower of the two middle samples (`lo` in
// dydx parlance) which matches the upstream Connect behaviour and avoids
// introducing a half-bit precision artefact.
func Median(values []*big.Int) *big.Int {
	if len(values) == 0 {
		return nil
	}
	cp := make([]*big.Int, len(values))
	copy(cp, values)
	sort.Slice(cp, func(i, j int) bool { return cp[i].Cmp(cp[j]) < 0 })
	return new(big.Int).Set(cp[(len(cp)-1)/2])
}

// AggregateInputs is the per-pair samples consumed by Aggregate. The caller is
// responsible for deduplicating by (pair, provider) - the orchestrator keeps
// only the latest observation per (pair, provider) pair.
type AggregateInputs struct {
	Pair    types.CurrencyPair
	Samples []types.Price
}

// AggregateConfig controls how stale samples are filtered before aggregation.
type AggregateConfig struct {
	MaxAge      time.Duration
	MinSources  int
	NowOverride time.Time // for tests
}

// Aggregate runs cross-provider median per pair, returning the resulting
// snapshot keyed by canonical pair string ("BTC/USD"). Pairs that fail the
// freshness or min-source thresholds are silently dropped — the chain side is
// expected to publish an empty vote-extension for missing markets, mirroring
// dydx behaviour.
func Aggregate(inputs []AggregateInputs, cfg AggregateConfig) map[string]*big.Int {
	now := cfg.NowOverride
	if now.IsZero() {
		now = time.Now().UTC()
	}
	out := make(map[string]*big.Int, len(inputs))
	for _, in := range inputs {
		fresh := make([]*big.Int, 0, len(in.Samples))
		for _, s := range in.Samples {
			if cfg.MaxAge > 0 && now.Sub(s.Timestamp) > cfg.MaxAge {
				continue
			}
			if s.Value == nil || s.Value.Sign() <= 0 {
				continue
			}
			fresh = append(fresh, s.Value)
		}
		if len(fresh) == 0 {
			continue
		}
		if cfg.MinSources > 0 && len(fresh) < cfg.MinSources {
			continue
		}
		out[in.Pair.String()] = Median(fresh)
	}
	return out
}
