//go:build liveoracle
// +build liveoracle

// Suite: end-to-end daemon ↔ real sidecar binary.
//
// Gated behind the `liveoracle` build tag so CI doesn't hit Binance /
// OKX / CoinGecko on every push. Run locally with:
//
//	make build-sidecar && go test -tags liveoracle -count=1 ./x/oracle/tests/...
package tests

import (
	"context"
	"fmt"
	"testing"
	"time"

	"cosmossdk.io/log"
	"github.com/stretchr/testify/require"

	"github.com/perpdex/perpdex-l1/tests/livehelpers"
	"github.com/perpdex/perpdex-l1/x/oracle/daemon"
)

// TestDaemonLivePollAgainstSidecar exercises the *real* sidecar binary
// (built via `make build-sidecar`) end-to-end against the in-process
// daemon goroutine. It asserts that:
//
//  1. The daemon successfully dials the sidecar's freshly-bound gRPC port.
//  2. After a few poll ticks (interval=500ms) all three configured pairs
//     end up in the cache with non-zero prices.
//  3. The daemon's `AsPriceFetcher` adapter returns the same set when
//     queried like the keeper does on `ExtendVote`.
func TestDaemonLivePollAgainstSidecar(t *testing.T) {
	h := livehelpers.StartSidecar(t, livehelpers.DefaultLiveConfig(), 15*time.Second)

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	// We don't have a chain process here so we drive the resolver
	// directly. Decimals=2 mirrors `daemon/adapter.go:defaultPriceDecimals`
	// so scalePrice's overflow path is taken on the same code path the
	// chain hits.
	d := daemon.New(log.NewTestLogger(t), daemon.Config{
		SidecarAddress:  h.GRPCAddr,
		FetchInterval:   200 * time.Millisecond,
		FetchTimeout:    1 * time.Second,
		SidecarDecimals: 8,
		MaxAge:          5 * time.Second,
		Enabled:         true,
	}, nil, nil)
	d.Resolver().Set("BTC/USD", 1, 2)
	d.Resolver().Set("ETH/USD", 2, 2)
	d.Resolver().Set("SOL/USD", 3, 2)

	require.NoError(t, d.Start(ctx))
	t.Cleanup(d.Stop)

	require.NoError(t, livehelpers.WaitFor(ctx, 15*time.Second, func() error {
		snap := d.Cache().Snapshot(time.Now().UTC(), 5*time.Second)
		if len(snap) < 3 {
			return fmt.Errorf("only %d markets cached, want 3", len(snap))
		}
		seen := map[uint32]bool{}
		for _, p := range snap {
			if p.IndexPrice == 0 || p.MarkPrice == 0 {
				return fmt.Errorf("market %d still zero", p.MarketIndex)
			}
			seen[p.MarketIndex] = true
		}
		for _, idx := range []uint32{1, 2, 3} {
			if !seen[idx] {
				return fmt.Errorf("market_index %d missing from cache", idx)
			}
		}
		return nil
	}))

	fetcher := d.AsPriceFetcher()
	got, err := fetcher.FetchPrices(ctx, 100)
	require.NoError(t, err)
	require.Len(t, got, 3, "AsPriceFetcher must reflect cache contents")

	// Sanity-check the scaling: at 2 decimals BTC/USD should land north
	// of $1000 (i.e. > 100 000 in chain units), comfortably below the
	// uint32 ceiling. We pin a generous lower bound rather than a
	// brittle exact value so the test keeps working through actual
	// market moves.
	for _, p := range got {
		switch p.MarketIndex {
		case 1: // BTC/USD * 100
			require.Greater(t, p.IndexPrice, uint32(100_000), "BTC must exceed $1000")
		case 2: // ETH/USD * 100
			require.Greater(t, p.IndexPrice, uint32(10_000), "ETH must exceed $100")
		case 3: // SOL/USD * 100
			require.Greater(t, p.IndexPrice, uint32(100), "SOL must exceed $1")
		}
		require.Equal(t, p.IndexPrice, p.MarkPrice, "daemon writes index==mark; smoothing happens chain-side")
	}

	require.False(t, d.LastUpdate().IsZero(), "LastUpdate should reflect at least one successful poll")
}
