// Package metrics exposes the sidecar's Prometheus registry under a dedicated
// HTTP handler so operators can scrape provider liveness, snapshot freshness,
// and per-pair sample counts without going through the gRPC surface.
package metrics

import (
	"context"
	"fmt"
	"net/http"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

// Registry is the global registry the sidecar feeds metrics into. It is kept
// package-scoped so providers can register counters during init() without
// having to thread a registry pointer through their constructors.
var Registry = prometheus.NewRegistry()

// SnapshotFreshness reports the age (in seconds) of the most recent
// orchestrator snapshot. Operators alert on this to detect upstream provider
// failures.
var SnapshotFreshness = prometheus.NewGauge(prometheus.GaugeOpts{
	Namespace: "perpdex_oracle_sidecar",
	Name:      "snapshot_age_seconds",
	Help:      "Age of the most recent aggregated snapshot in seconds.",
})

// PairsObserved is the cardinality of the most recent aggregated snapshot.
// A drop here is the canary that indicates one or more providers stopped
// publishing for a pair.
var PairsObserved = prometheus.NewGauge(prometheus.GaugeOpts{
	Namespace: "perpdex_oracle_sidecar",
	Name:      "pairs_observed",
	Help:      "Number of pairs present in the most recent aggregated snapshot.",
})

func init() {
	Registry.MustRegister(SnapshotFreshness, PairsObserved)
	Registry.MustRegister(prometheus.NewGoCollector())
	Registry.MustRegister(prometheus.NewProcessCollector(prometheus.ProcessCollectorOpts{}))
}

// Serve binds an HTTP server on addr exposing /metrics with the package
// registry. It blocks until ctx is cancelled or the server fails.
func Serve(ctx context.Context, addr string) error {
	mux := http.NewServeMux()
	mux.Handle("/metrics", promhttp.HandlerFor(Registry, promhttp.HandlerOpts{}))
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	})

	srv := &http.Server{
		Addr:              addr,
		Handler:           mux,
		ReadHeaderTimeout: 5 * time.Second,
	}
	go func() {
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		_ = srv.Shutdown(shutdownCtx)
	}()
	if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		return fmt.Errorf("metrics: %w", err)
	}
	return nil
}
