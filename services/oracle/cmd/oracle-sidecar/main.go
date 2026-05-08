// Command oracle-sidecar runs the perpdex-l1 oracle price sidecar.
//
// It reads configuration from the path passed via --config (or stdin if
// `-`), spins up the configured price providers, and exposes a gRPC `Oracle`
// service the chain daemon polls every block. It is intentionally a single,
// short-lived process — the chain process supervisor (systemd, kubernetes,
// docker-compose) is responsible for restart/lifecycle management.
package main

import (
	"context"
	"flag"
	"log"
	"math/big"
	"os"
	"os/signal"
	"runtime/debug"
	"syscall"
	"time"

	"github.com/perpdex/perpdex-l1/oracle-sidecar/config"
	"github.com/perpdex/perpdex-l1/oracle-sidecar/metrics"
	"github.com/perpdex/perpdex-l1/oracle-sidecar/oracle"
	"github.com/perpdex/perpdex-l1/oracle-sidecar/providers/binance"
	"github.com/perpdex/perpdex-l1/oracle-sidecar/providers/coingecko"
	"github.com/perpdex/perpdex-l1/oracle-sidecar/providers/okx"
	"github.com/perpdex/perpdex-l1/oracle-sidecar/providers/types"
	"github.com/perpdex/perpdex-l1/oracle-sidecar/service"
)

var (
	cfgPath    = flag.String("config", "", "path to oracle.json config (empty = built-in defaults)")
	logVerbose = flag.Bool("v", false, "verbose logging")
)

func main() {
	flag.Parse()

	cfg, err := config.Load(*cfgPath)
	if err != nil {
		log.Fatalf("load config: %v", err)
	}

	logger := log.New(os.Stderr, "[oracle-sidecar] ", log.LstdFlags|log.Lmicroseconds)
	if *logVerbose {
		logger.Printf("config: %+v", cfg)
	}

	pairs, err := parsePairs(cfg.Pairs)
	if err != nil {
		log.Fatalf("parse pairs: %v", err)
	}

	providers, err := buildProviders(cfg, pairs)
	if err != nil {
		log.Fatalf("build providers: %v", err)
	}
	if len(providers) == 0 {
		log.Fatalf("no enabled providers in config")
	}

	orch := oracle.New(logger, providers, oracle.AggregateConfig{
		MaxAge:     time.Duration(cfg.MaxAge),
		MinSources: cfg.MinSources,
	})

	rootCtx, cancel := context.WithCancel(context.Background())
	defer cancel()

	go signalLoop(cancel, logger)

	go func() {
		if err := orch.Run(rootCtx); err != nil && rootCtx.Err() == nil {
			logger.Printf("orchestrator exited: %v", err)
			cancel()
		}
	}()

	go func() {
		if err := metrics.Serve(rootCtx, cfg.MetricsAddr); err != nil && rootCtx.Err() == nil {
			logger.Printf("metrics: %v", err)
		}
	}()

	go freshnessReporter(rootCtx, orch)

	srv := service.NewServer(orch, service.BuildInfo{
		Version:   buildVersion(),
		BuildDate: time.Now().UTC().Format(time.RFC3339),
	})
	logger.Printf("starting gRPC server on %s", cfg.GRPCAddr)
	if err := service.Serve(rootCtx, cfg.GRPCAddr, srv); err != nil && rootCtx.Err() == nil {
		log.Fatalf("grpc serve: %v", err)
	}
}

func parsePairs(raw []string) ([]types.CurrencyPair, error) {
	out := make([]types.CurrencyPair, 0, len(raw))
	for _, s := range raw {
		p, err := types.ParseCurrencyPair(s)
		if err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, nil
}

func buildProviders(cfg *config.Config, pairs []types.CurrencyPair) ([]types.Provider, error) {
	var providers []types.Provider

	if pc, ok := cfg.Providers["binance"]; ok && pc.Enabled {
		providers = append(providers, binance.New(binance.Config{
			Endpoint: pc.Endpoint,
			Interval: time.Duration(pc.Interval),
			Timeout:  time.Duration(pc.Timeout),
			Decimals: pc.Decimals,
			Pairs:    pairs,
		}))
	}
	if pc, ok := cfg.Providers["okx"]; ok && pc.Enabled {
		providers = append(providers, okx.New(okx.Config{
			Endpoint: pc.Endpoint,
			Interval: time.Duration(pc.Interval),
			Timeout:  time.Duration(pc.Timeout),
			Decimals: pc.Decimals,
			Pairs:    pairs,
		}))
	}
	if pc, ok := cfg.Providers["coingecko"]; ok && pc.Enabled {
		cg, err := coingecko.New(coingecko.Config{
			Endpoint: pc.Endpoint,
			APIKey:   pc.APIKey,
			Interval: time.Duration(pc.Interval),
			Timeout:  time.Duration(pc.Timeout),
			Decimals: pc.Decimals,
			Pairs:    pairs,
			Slugs:    pc.Slugs,
		})
		if err != nil {
			return nil, err
		}
		providers = append(providers, cg)
	}
	return providers, nil
}

func signalLoop(cancel context.CancelFunc, logger *log.Logger) {
	sig := make(chan os.Signal, 2)
	signal.Notify(sig, os.Interrupt, syscall.SIGTERM)
	s := <-sig
	logger.Printf("received signal %s, shutting down", s)
	cancel()
}

func freshnessReporter(ctx context.Context, orch *oracle.Orchestrator) {
	tick := time.NewTicker(2 * time.Second)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
			snap, ts := orch.Snapshot()
			if ts.IsZero() {
				continue
			}
			metrics.SnapshotFreshness.Set(time.Since(ts).Seconds())
			metrics.PairsObserved.Set(float64(len(snap)))
		}
	}
}

// buildVersion is reported by the Version RPC. We pull from runtime/debug so
// the binary embeds the VCS revision when built with -trimpath -buildvcs.
func buildVersion() string {
	if info, ok := debug.ReadBuildInfo(); ok {
		for _, s := range info.Settings {
			if s.Key == "vcs.revision" && len(s.Value) >= 7 {
				return "dev-" + s.Value[:7]
			}
		}
	}
	return "v0.0.0-dev"
}

// Compile-time assertion that *big.Int matches the snapshot value type. This
// is otherwise a redundant import; without it `go vet` complains about an
// unused import after future refactors.
var _ = (*big.Int)(nil)
