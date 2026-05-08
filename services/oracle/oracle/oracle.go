package oracle

import (
	"context"
	"fmt"
	"log"
	"math/big"
	"sync"
	"time"

	"github.com/perpdex/perpdex-l1/oracle-sidecar/providers/types"
)

// Orchestrator wires up the providers, runs them in goroutines, fans their
// observations through Aggregate, and exposes the latest snapshot for the
// gRPC server to read.
//
// It is the single source of truth for the sidecar's "latest aggregated
// prices" state and is safe for concurrent reads.
type Orchestrator struct {
	logger    *log.Logger
	providers []types.Provider
	cfg       AggregateConfig

	mu        sync.RWMutex
	latest    map[string]types.Price            // latest sample per (pair, provider)
	pairIndex map[string]types.CurrencyPair     // pair-string -> CurrencyPair
	snapshot  map[string]*big.Int               // latest aggregated snapshot
	updatedAt time.Time
}

// New constructs a non-running orchestrator. Call Run on a goroutine to start
// the providers and aggregation loop.
func New(logger *log.Logger, providers []types.Provider, cfg AggregateConfig) *Orchestrator {
	pairIndex := make(map[string]types.CurrencyPair)
	for _, p := range providers {
		for _, pr := range p.Pairs() {
			pairIndex[pr.String()] = pr
		}
	}
	return &Orchestrator{
		logger:    logger,
		providers: providers,
		cfg:       cfg,
		latest:    make(map[string]types.Price),
		pairIndex: pairIndex,
		snapshot:  make(map[string]*big.Int),
	}
}

// Run blocks until ctx is cancelled. It launches each provider in its own
// goroutine, drains a single aggregated price channel, and re-runs the median
// every aggregate-tick (default 250ms) so the snapshot is always fresh.
func (o *Orchestrator) Run(ctx context.Context) error {
	if len(o.providers) == 0 {
		return fmt.Errorf("oracle: no providers configured")
	}

	priceCh := make(chan []types.Price, 256)
	var wg sync.WaitGroup
	for _, p := range o.providers {
		p := p
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := p.Start(ctx, priceCh); err != nil && ctx.Err() == nil {
				o.logger.Printf("provider %q exited: %v", p.Name(), err)
			}
		}()
	}

	aggregateInterval := 250 * time.Millisecond
	tick := time.NewTicker(aggregateInterval)
	defer tick.Stop()

	for {
		select {
		case <-ctx.Done():
			wg.Wait()
			return ctx.Err()
		case batch := <-priceCh:
			o.ingest(batch)
		case <-tick.C:
			o.recomputeSnapshot()
		}
	}
}

// ingest stores the most recent (pair, provider) sample. We keep one slot per
// (pair, provider) tuple so a flapping provider cannot starve the median by
// pushing rapid duplicates.
func (o *Orchestrator) ingest(batch []types.Price) {
	if len(batch) == 0 {
		return
	}
	o.mu.Lock()
	defer o.mu.Unlock()
	for _, sample := range batch {
		key := sample.Pair.String() + "@" + sample.Provider
		o.latest[key] = sample
	}
}

func (o *Orchestrator) recomputeSnapshot() {
	o.mu.Lock()
	defer o.mu.Unlock()

	bucket := make(map[string][]types.Price, len(o.pairIndex))
	for _, sample := range o.latest {
		k := sample.Pair.String()
		bucket[k] = append(bucket[k], sample)
	}
	inputs := make([]AggregateInputs, 0, len(bucket))
	for k, samples := range bucket {
		inputs = append(inputs, AggregateInputs{
			Pair:    o.pairIndex[k],
			Samples: samples,
		})
	}
	o.snapshot = Aggregate(inputs, o.cfg)
	o.updatedAt = time.Now().UTC()
}

// Snapshot returns a deep copy of the latest aggregated price set. The returned
// map is safe for the caller to mutate.
func (o *Orchestrator) Snapshot() (map[string]*big.Int, time.Time) {
	o.mu.RLock()
	defer o.mu.RUnlock()
	out := make(map[string]*big.Int, len(o.snapshot))
	for k, v := range o.snapshot {
		out[k] = new(big.Int).Set(v)
	}
	return out, o.updatedAt
}
