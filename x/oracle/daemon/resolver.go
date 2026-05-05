package daemon

import (
	"context"
	"fmt"
	"strings"
	"sync"

	"github.com/perpdex/perpdex-l1/x/oracle/types"
)

// MarketResolver translates the canonical "BASE/QUOTE" pair-strings the
// sidecar produces (e.g. "BTC/USD") into perpdex-l1 `market_index` values and
// per-market price precision (decimals).
//
// It is built fresh on every daemon tick (see Daemon.Refresh) by walking the
// markets and assets keepers — perpdex governance can register new markets
// at any time and we don't want a stale resolver hiding new tickers from the
// price feed.
type MarketResolver struct {
	mu        sync.RWMutex
	pairToIdx map[string]uint32
	idxToPair map[uint32]string
	decimals  map[uint32]uint8
}

// NewMarketResolver returns an empty resolver. Use Refresh or Set to populate.
func NewMarketResolver() *MarketResolver {
	return &MarketResolver{
		pairToIdx: make(map[string]uint32),
		idxToPair: make(map[uint32]string),
		decimals:  make(map[uint32]uint8),
	}
}

// PairFromAssets builds a canonical pair string from base and quote asset
// display names. The chain stores symbols however the asset DisplayName is
// configured (e.g. "BTC", "USDT") — we upper-case both sides so lookups are
// case-insensitive.
func PairFromAssets(base, quote string) string {
	base = strings.ToUpper(strings.TrimSpace(base))
	quote = strings.ToUpper(strings.TrimSpace(quote))
	// Normalise USDT -> USD so "BTC/USDT" maps to the same canonical key as
	// the sidecar's BTC/USD output. Operators who specifically want to track
	// USDT can disable this by registering the market with quote symbol
	// "USDT-NORMALIZED" or similar.
	if quote == "USDT" {
		quote = "USD"
	}
	return base + "/" + quote
}

// MarketReader is the read-only subset of x/market keeper exposed to the
// daemon. The method shape mirrors what's already on the keeper so we don't
// have to define a brand-new interface.
type MarketReader interface {
	IterateMarkets(ctx context.Context, cb func(types.MarketShim) bool) error
}

// AssetReader is the read-only subset of x/asset keeper exposed to the daemon.
type AssetReader interface {
	GetAssetByIndex(ctx context.Context, index uint32) (string, uint32, error)
}

// Refresh walks all markets via mr, looks up base/quote display-names via ar,
// and replaces the resolver's internal maps atomically.
func (r *MarketResolver) Refresh(ctx context.Context, mr MarketReader, ar AssetReader) error {
	pairToIdx := make(map[string]uint32)
	idxToPair := make(map[uint32]string)
	decimals := make(map[uint32]uint8)

	if err := mr.IterateMarkets(ctx, func(m types.MarketShim) bool {
		baseSym, _, errB := ar.GetAssetByIndex(ctx, m.BaseAssetID)
		quoteSym, _, errQ := ar.GetAssetByIndex(ctx, m.QuoteAssetID)
		if errB != nil || errQ != nil || baseSym == "" || quoteSym == "" {
			return false
		}
		pair := PairFromAssets(baseSym, quoteSym)
		pairToIdx[pair] = m.MarketIndex
		idxToPair[m.MarketIndex] = pair
		decimals[m.MarketIndex] = m.Decimals
		return false
	}); err != nil {
		return fmt.Errorf("oracle daemon: iterate markets: %w", err)
	}

	r.mu.Lock()
	defer r.mu.Unlock()
	r.pairToIdx = pairToIdx
	r.idxToPair = idxToPair
	r.decimals = decimals
	return nil
}

// MarketIndex returns the on-chain index for `pair`. ok==false means the
// pair has not been registered as a market yet — callers SHOULD silently
// drop the sidecar quote in that case.
func (r *MarketResolver) MarketIndex(pair string) (uint32, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	idx, ok := r.pairToIdx[strings.ToUpper(pair)]
	return idx, ok
}

// Decimals returns the chain-side precision for the given market_index.
// Falls back to the sidecar default of 8 decimals when the market has no
// explicit decimals configured (zero is treated as "use default").
func (r *MarketResolver) Decimals(idx uint32) uint8 {
	r.mu.RLock()
	defer r.mu.RUnlock()
	d, ok := r.decimals[idx]
	if !ok || d == 0 {
		return 8
	}
	return d
}

// Pair returns the canonical pair string for a market_index. Useful for
// log messages and metrics labels.
func (r *MarketResolver) Pair(idx uint32) string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.idxToPair[idx]
}

// Set is a low-level test/inject helper. Production code should rely on
// Refresh. It replaces a single mapping atomically.
func (r *MarketResolver) Set(pair string, idx uint32, decimals uint8) {
	pair = strings.ToUpper(pair)
	r.mu.Lock()
	defer r.mu.Unlock()
	r.pairToIdx[pair] = idx
	r.idxToPair[idx] = pair
	if decimals > 0 {
		r.decimals[idx] = decimals
	}
}

// Size returns the number of registered markets.
func (r *MarketResolver) Size() int {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return len(r.pairToIdx)
}
