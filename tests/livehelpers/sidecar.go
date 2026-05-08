// Package livehelpers provides shared scaffolding for `liveoracle`-tagged
// integration tests that need to spawn the real `oracle-sidecar` binary.
//
// The helpers here intentionally avoid any `cosmos-sdk` imports so a single
// utility can be reused from both `x/oracle/daemon` (lower-level daemon
// loopback test) and `tests/e2e` (full ABCI++ pipeline test) without pulling
// in heavyweight dependencies for the regular unit-test build.
package livehelpers

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"testing"
	"time"
)

// SidecarConfig is the subset of the `oracle.json` schema we re-serialise
// when launching the sidecar with a freshly chosen port. We only need the
// fields the binary actually reads at startup; the structure shadows
// `oracle-sidecar/config.Config` to avoid an import cycle (the helper sits
// outside the sidecar's go.mod path).
type SidecarConfig struct {
	GRPCAddr    string                          `json:"grpc_addr"`
	MetricsAddr string                          `json:"metrics_addr"`
	MaxAge      string                          `json:"max_age"`
	MinSources  int                             `json:"min_sources"`
	Pairs       []string                        `json:"pairs"`
	Providers   map[string]SidecarProviderEntry `json:"providers"`
}

// SidecarProviderEntry mirrors `config.ProviderConfig` (only the fields we
// pre-populate from the bundled `oracle.json`).
type SidecarProviderEntry struct {
	Enabled  bool              `json:"enabled"`
	Endpoint string            `json:"endpoint,omitempty"`
	Interval string            `json:"interval,omitempty"`
	Timeout  string            `json:"timeout,omitempty"`
	Decimals uint8             `json:"decimals,omitempty"`
	Slugs    map[string]string `json:"slugs,omitempty"`
}

// DefaultLiveConfig returns a sidecar config equivalent to the bundled
// `services/oracle/oracle.json` but with `grpc_addr` / `metrics_addr` left
// blank so callers can fill in dynamically chosen ports.
func DefaultLiveConfig() SidecarConfig {
	return SidecarConfig{
		MaxAge:     "5s",
		MinSources: 1,
		Pairs:      []string{"BTC/USD", "ETH/USD", "SOL/USD"},
		Providers: map[string]SidecarProviderEntry{
			"binance":   {Enabled: true, Interval: "1500ms", Timeout: "1s", Decimals: 8},
			"okx":       {Enabled: true, Interval: "1500ms", Timeout: "1s", Decimals: 8},
			"coingecko": {Enabled: true, Interval: "5s", Timeout: "3s", Decimals: 8, Slugs: map[string]string{"BTC": "bitcoin", "ETH": "ethereum", "SOL": "solana"}},
		},
	}
}

// SidecarHandle bundles the metadata returned by Start so callers can
// connect a daemon and tear the process down on test exit.
type SidecarHandle struct {
	GRPCAddr    string
	MetricsAddr string
	cmd         *exec.Cmd
}

// Stop terminates the sidecar process, ignoring "already exited" errors.
func (h *SidecarHandle) Stop() {
	if h == nil || h.cmd == nil || h.cmd.Process == nil {
		return
	}
	_ = h.cmd.Process.Signal(os.Interrupt)
	doneCh := make(chan struct{})
	go func() {
		_ = h.cmd.Wait()
		close(doneCh)
	}()
	select {
	case <-doneCh:
	case <-time.After(3 * time.Second):
		_ = h.cmd.Process.Kill()
		<-doneCh
	}
}

// RepoRoot walks up from the live-helpers source file to the repository
// root. We deliberately use `runtime.Caller` instead of an env-var so live
// integration tests work out of the box from any cwd.
func RepoRoot(t *testing.T) string {
	t.Helper()
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("livehelpers: runtime.Caller failed")
	}
	root := filepath.Clean(filepath.Join(filepath.Dir(file), "..", ".."))
	if _, err := os.Stat(filepath.Join(root, "go.mod")); err != nil {
		t.Fatalf("livehelpers: derived repo root %q has no go.mod: %v", root, err)
	}
	return root
}

// SidecarBinaryPath resolves the location of the prebuilt sidecar binary,
// preferring an `ORACLE_SIDECAR_BIN` override over the canonical
// `<repo>/services/oracle/build/oracle-sidecar` path produced by
// `make build-sidecar`.
func SidecarBinaryPath(t *testing.T) string {
	t.Helper()
	if v := os.Getenv("ORACLE_SIDECAR_BIN"); v != "" {
		if _, err := os.Stat(v); err != nil {
			t.Fatalf("livehelpers: ORACLE_SIDECAR_BIN=%q not found: %v", v, err)
		}
		return v
	}
	bin := filepath.Join(RepoRoot(t), "services", "oracle", "build", "oracle-sidecar")
	if _, err := os.Stat(bin); err != nil {
		t.Fatalf("livehelpers: %s missing — run `make build-sidecar` first (or set ORACLE_SIDECAR_BIN)", bin)
	}
	return bin
}

// pickFreePort binds 127.0.0.1:0, immediately closes the listener, and
// returns the kernel-assigned port. The microscopic race window between
// close + sidecar bind is acceptable for tests; if it ever bites we can
// switch to fd-passing (SO_REUSEPORT is not portable to macOS without
// extra ceremony).
func pickFreePort(t *testing.T) int {
	t.Helper()
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("livehelpers: pick free port: %v", err)
	}
	port := lis.Addr().(*net.TCPAddr).Port
	_ = lis.Close()
	return port
}

// StartSidecar launches the sidecar binary against a fresh tempdir-stored
// config with dynamically chosen gRPC + metrics ports. It blocks until the
// gRPC port is reachable (or `readyTimeout` elapses, whichever comes
// first).
//
// The returned handle's Stop method is registered with t.Cleanup so test
// authors usually only need:
//
//	h := livehelpers.StartSidecar(t, livehelpers.DefaultLiveConfig(), 15*time.Second)
//	... daemon.NewSidecarClient(daemon.ClientConfig{Address: h.GRPCAddr})
func StartSidecar(t *testing.T, cfg SidecarConfig, readyTimeout time.Duration) *SidecarHandle {
	t.Helper()

	if cfg.GRPCAddr == "" {
		cfg.GRPCAddr = fmt.Sprintf("127.0.0.1:%d", pickFreePort(t))
	}
	if cfg.MetricsAddr == "" {
		cfg.MetricsAddr = fmt.Sprintf("127.0.0.1:%d", pickFreePort(t))
	}

	bin := SidecarBinaryPath(t)
	tmp := t.TempDir()
	cfgPath := filepath.Join(tmp, "oracle.json")
	bz, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		t.Fatalf("livehelpers: marshal config: %v", err)
	}
	if err := os.WriteFile(cfgPath, bz, 0o600); err != nil {
		t.Fatalf("livehelpers: write %s: %v", cfgPath, err)
	}

	stdout, err := os.Create(filepath.Join(tmp, "sidecar.stdout.log"))
	if err != nil {
		t.Fatalf("livehelpers: create stdout log: %v", err)
	}
	stderr, err := os.Create(filepath.Join(tmp, "sidecar.stderr.log"))
	if err != nil {
		t.Fatalf("livehelpers: create stderr log: %v", err)
	}

	cmd := exec.Command(bin, "--config", cfgPath, "-v")
	cmd.Stdout = stdout
	cmd.Stderr = stderr

	if err := cmd.Start(); err != nil {
		t.Fatalf("livehelpers: start %s: %v", bin, err)
	}

	h := &SidecarHandle{
		GRPCAddr:    cfg.GRPCAddr,
		MetricsAddr: cfg.MetricsAddr,
		cmd:         cmd,
	}
	t.Cleanup(func() {
		h.Stop()
		_ = stdout.Close()
		_ = stderr.Close()
		if t.Failed() {
			dumpFile(t, stdout.Name(), "sidecar stdout")
			dumpFile(t, stderr.Name(), "sidecar stderr")
		}
	})

	if err := waitForTCP(cfg.GRPCAddr, readyTimeout); err != nil {
		dumpFile(t, stderr.Name(), "sidecar stderr")
		t.Fatalf("livehelpers: sidecar gRPC %s did not become ready: %v", cfg.GRPCAddr, err)
	}
	return h
}

// WaitFor repeatedly invokes `predicate` until it returns nil or `timeout`
// elapses. It is a tiny helper used by both phase-2 and phase-3 tests to
// assert "the cache eventually has N entries" without resorting to ad-hoc
// `time.Sleep` calls.
func WaitFor(ctx context.Context, timeout time.Duration, predicate func() error) error {
	deadline := time.Now().Add(timeout)
	for {
		err := predicate()
		if err == nil {
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("livehelpers: timed out after %s: %w", timeout, err)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(100 * time.Millisecond):
		}
	}
}

func waitForTCP(addr string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for {
		conn, err := net.DialTimeout("tcp", addr, 250*time.Millisecond)
		if err == nil {
			_ = conn.Close()
			return nil
		}
		if time.Now().After(deadline) {
			return err
		}
		time.Sleep(100 * time.Millisecond)
	}
}

func dumpFile(t *testing.T, path, label string) {
	t.Helper()
	bz, err := os.ReadFile(path)
	if err != nil {
		return
	}
	if len(bz) == 0 {
		return
	}
	t.Logf("--- %s (%s) ---\n%s", label, path, string(bz))
}
