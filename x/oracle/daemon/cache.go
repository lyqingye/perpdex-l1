// Package daemon implements the chain-side counterpart of the oracle sidecar.
//
// It runs in the chain process as a single goroutine, polls the local
// oracle-sidecar's gRPC `Oracle.Prices` endpoint at a fixed cadence, resolves
// the returned currency-pair strings (e.g. "BTC/USD") against the on-chain
// market registry, and stores the result in a thread-safe in-memory cache
// that the VE `ExtendVote` handler reads on every prevote.
//
// The daemon is independent of the consensus loop: ABCI handlers never block
// on a sidecar round-trip. This is the same architectural split dydx v4 uses
// (see protocol/daemons/slinky/client/client.go upstream).
package daemon

import (
	"sync"
	"time"

	"github.com/perpdex/perpdex-l1/x/oracle/types"
)

// CachedPrice is the per-market entry the daemon writes after each successful
// sidecar poll. The chain side stores `IndexPrice` and `MarkPrice` as the same
// observation — perpdex's index/mark distinction is enforced on-chain via EMA
// smoothing in the keeper, not by the upstream price source.
type CachedPrice struct {
	MarketIndex uint32
	Price       uint32
	UpdatedAt   time.Time
}

// Cache is a thread-safe map from market_index to the last sidecar
// observation. Reads are RWMutex-protected so ExtendVote, which runs on the
// consensus goroutine, never blocks behind a writer.
type Cache struct {
	mu    sync.RWMutex
	store map[uint32]CachedPrice
}

func NewCache() *Cache {
	return &Cache{store: make(map[uint32]CachedPrice)}
}

// Set replaces the cached price for `marketIndex`. Negative or zero `price`
// is silently dropped — the chain side treats absence and zero as identical
// "no price" signals.
func (c *Cache) Set(marketIndex uint32, price uint32, updatedAt time.Time) {
	if price == 0 {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.store[marketIndex] = CachedPrice{
		MarketIndex: marketIndex,
		Price:       price,
		UpdatedAt:   updatedAt,
	}
}

// Get returns the cached price for `marketIndex` and whether the entry
// exists. The returned CachedPrice is a value-copy; callers may mutate it
// without affecting the cache.
func (c *Cache) Get(marketIndex uint32) (CachedPrice, bool) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	v, ok := c.store[marketIndex]
	return v, ok
}

// Snapshot returns the freshest entry per market as a slice suitable for
// constructing the OracleVote vote-extension. Stale entries (older than
// maxAge) are filtered out; pass 0 to disable the filter.
func (c *Cache) Snapshot(now time.Time, maxAge time.Duration) []types.MarketPrice {
	c.mu.RLock()
	defer c.mu.RUnlock()
	out := make([]types.MarketPrice, 0, len(c.store))
	for _, v := range c.store {
		if maxAge > 0 && now.Sub(v.UpdatedAt) > maxAge {
			continue
		}
		out = append(out, types.MarketPrice{
			MarketIndex: v.MarketIndex,
			IndexPrice:  v.Price,
			MarkPrice:   v.Price,
		})
	}
	return out
}

// Size returns the number of entries currently held. Intended for metrics
// and tests, not for hot-path use.
func (c *Cache) Size() int {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return len(c.store)
}
