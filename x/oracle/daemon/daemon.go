package daemon

import (
	"context"
	"errors"
	"fmt"
	"math/big"
	"sync"
	"sync/atomic"
	"time"

	"cosmossdk.io/log"
)

// Config wires the daemon up. Every field has a sane default if left zero.
type Config struct {
	// SidecarAddress is the gRPC dial-string of the local oracle-sidecar.
	// Default "localhost:8080".
	SidecarAddress string
	// FetchInterval is how often the daemon polls the sidecar. Default 500ms.
	FetchInterval time.Duration
	// FetchTimeout caps a single Prices RPC. Default 200ms.
	FetchTimeout time.Duration
	// SidecarDecimals is the integer-encoding precision the sidecar uses
	// when serialising prices. Default 8 (matches the bundled
	// oracle-sidecar binary).
	SidecarDecimals uint8
	// MaxAge sets the freshness window the cache snapshot honours when
	// ExtendVote reads it. Default 5s.
	MaxAge time.Duration
	// Enabled lets operators short-circuit the daemon entirely (e.g. on a
	// non-validator full node). Defaults to false so non-validator full
	// nodes (and the e2e test rig) do not silently spawn a goroutine that
	// races against teardown; validator operators MUST set
	// `oracle.enabled = true` in app.toml.
	Enabled bool
}

// DefaultConfig returns a config suitable for a dev validator on the
// same host as the bundled sidecar.
func DefaultConfig() Config {
	return Config{
		SidecarAddress:  "localhost:8080",
		FetchInterval:   500 * time.Millisecond,
		FetchTimeout:    200 * time.Millisecond,
		SidecarDecimals: 8,
		MaxAge:          5 * time.Second,
		Enabled:         false,
	}
}

// Daemon is the long-lived chain-side companion to the oracle sidecar. It is
// constructed at app build time and started once the chain has finished
// loading (typically just before SetExtendVoteHandler is called).
type Daemon struct {
	cfg      Config
	logger   log.Logger
	cache    *Cache
	resolver *MarketResolver

	client *SidecarClient

	mr MarketReader
	ar AssetReader

	mu       sync.Mutex
	started  atomic.Bool
	cancelFn context.CancelFunc
	doneCh   chan struct{}

	lastUpdate atomic.Int64 // unix nanos of the last successful poll
}

// New constructs a Daemon. mr and ar may be nil when the daemon is started
// in a unit-test fixture; in that case the resolver is expected to be
// preloaded via Resolver().Set(...).
func New(logger log.Logger, cfg Config, mr MarketReader, ar AssetReader) *Daemon {
	if logger == nil {
		logger = log.NewNopLogger()
	}
	if cfg.SidecarAddress == "" {
		cfg = DefaultConfig()
	}
	if cfg.FetchInterval <= 0 {
		cfg.FetchInterval = 500 * time.Millisecond
	}
	if cfg.FetchTimeout <= 0 {
		cfg.FetchTimeout = 200 * time.Millisecond
	}
	if cfg.SidecarDecimals == 0 {
		cfg.SidecarDecimals = 8
	}
	if cfg.MaxAge <= 0 {
		cfg.MaxAge = 5 * time.Second
	}
	return &Daemon{
		cfg:      cfg,
		logger:   logger.With("module", "oracle-daemon"),
		cache:    NewCache(),
		resolver: NewMarketResolver(),
		mr:       mr,
		ar:       ar,
	}
}

// Cache returns the underlying price cache. ExtendVote reads from this.
func (d *Daemon) Cache() *Cache { return d.cache }

// Resolver returns the underlying market resolver. Useful for tests and ABCI
// modules that want to map a market_index back to a pair string.
func (d *Daemon) Resolver() *MarketResolver { return d.resolver }

// Enabled reports whether the daemon will attempt to dial the sidecar.
func (d *Daemon) Enabled() bool { return d.cfg.Enabled }

// LastUpdate returns the wall-clock time of the most recent successful poll.
// A zero time indicates the daemon has never received a price.
func (d *Daemon) LastUpdate() time.Time {
	v := d.lastUpdate.Load()
	if v == 0 {
		return time.Time{}
	}
	return time.Unix(0, v)
}

// Start dials the sidecar and runs the polling loop on its own goroutine.
// It returns immediately. Calling Start more than once is a no-op. Stop
// must be called to release the gRPC connection at process shutdown.
//
// If the daemon is configured with `Enabled=false` Start is a successful
// no-op so non-validator nodes can run the chain without any sidecar
// dependency.
func (d *Daemon) Start(parent context.Context) error {
	if !d.started.CompareAndSwap(false, true) {
		return nil
	}
	if !d.cfg.Enabled {
		d.logger.Info("oracle daemon disabled by config; ExtendVote will produce empty payloads")
		return nil
	}
	d.mu.Lock()
	defer d.mu.Unlock()

	cli, err := NewSidecarClient(ClientConfig{
		Address: d.cfg.SidecarAddress,
		Timeout: d.cfg.FetchTimeout,
	})
	if err != nil {
		// Refusing to start the chain because the sidecar isn't dialable
		// would be unhelpful — the dial is lazy in gRPC, so we surface
		// the error in metrics/logs instead.
		d.logger.Error("oracle daemon failed to construct sidecar client", "err", err)
		d.started.Store(false)
		return nil
	}
	d.client = cli

	ctx, cancel := context.WithCancel(parent)
	d.cancelFn = cancel
	d.doneCh = make(chan struct{})
	go d.run(ctx)
	d.logger.Info("oracle daemon started", "sidecar", d.cfg.SidecarAddress, "interval", d.cfg.FetchInterval.String())
	return nil
}

// Stop signals the goroutine to exit and blocks until the gRPC connection is
// closed. Safe to call from any goroutine; idempotent.
func (d *Daemon) Stop() {
	d.mu.Lock()
	cancel := d.cancelFn
	done := d.doneCh
	d.cancelFn = nil
	d.doneCh = nil
	cli := d.client
	d.client = nil
	d.mu.Unlock()

	if cancel != nil {
		cancel()
	}
	if done != nil {
		<-done
	}
	if cli != nil {
		_ = cli.Close()
	}
	d.started.Store(false)
}

func (d *Daemon) run(ctx context.Context) {
	defer close(d.doneCh)

	if v := d.client.ProbeVersion(ctx); v != "" {
		d.logger.Info("oracle sidecar version probe", "version", v)
	}

	tick := time.NewTicker(d.cfg.FetchInterval)
	defer tick.Stop()

	if err := d.refreshResolver(ctx); err != nil {
		d.logger.Debug("oracle resolver initial refresh failed", "err", err)
	}
	if err := d.poll(ctx); err != nil {
		d.logger.Debug("oracle daemon initial poll failed", "err", err)
	}

	resolverTick := time.NewTicker(10 * time.Second)
	defer resolverTick.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-resolverTick.C:
			if err := d.refreshResolver(ctx); err != nil {
				d.logger.Debug("oracle resolver refresh failed", "err", err)
			}
		case <-tick.C:
			if err := d.poll(ctx); err != nil {
				if !errors.Is(err, context.Canceled) {
					d.logger.Debug("oracle daemon poll failed", "err", err)
				}
			}
		}
	}
}

func (d *Daemon) poll(ctx context.Context) error {
	quotes, err := d.client.FetchPrices(ctx)
	if err != nil {
		return err
	}
	now := time.Now().UTC()
	for _, q := range quotes {
		idx, ok := d.resolver.MarketIndex(q.Pair)
		if !ok {
			continue
		}
		dec := d.resolver.Decimals(idx)
		scaled, ok := scalePrice(q.Value, d.cfg.SidecarDecimals, dec)
		if !ok {
			d.logger.Debug("oracle daemon: price overflows uint32", "pair", q.Pair, "raw", q.Value.String())
			continue
		}
		d.cache.Set(idx, scaled, now)
	}
	d.lastUpdate.Store(now.UnixNano())
	return nil
}

func (d *Daemon) refreshResolver(ctx context.Context) error {
	if d.mr == nil || d.ar == nil {
		return nil
	}
	return d.resolver.Refresh(ctx, d.mr, d.ar)
}

// scalePrice converts an integer-encoded sidecar price (`raw`, with
// `srcDecimals` decimal places) into a chain-side uint32 price using
// `dstDecimals` decimal places. Returns ok=false when the result overflows
// uint32 — the chain's `OraclePrice.IndexPrice` and `MarkPrice` are uint32.
func scalePrice(raw *big.Int, srcDecimals, dstDecimals uint8) (uint32, bool) {
	if raw == nil || raw.Sign() <= 0 {
		return 0, false
	}
	if srcDecimals == dstDecimals {
		if !raw.IsUint64() {
			return 0, false
		}
		v := raw.Uint64()
		if v > uint64(^uint32(0)) {
			return 0, false
		}
		return uint32(v), true
	}
	out := new(big.Int).Set(raw)
	if dstDecimals > srcDecimals {
		mul := new(big.Int).Exp(big.NewInt(10), big.NewInt(int64(dstDecimals-srcDecimals)), nil)
		out.Mul(out, mul)
	} else {
		div := new(big.Int).Exp(big.NewInt(10), big.NewInt(int64(srcDecimals-dstDecimals)), nil)
		out.Quo(out, div)
	}
	if !out.IsUint64() {
		return 0, false
	}
	v := out.Uint64()
	if v > uint64(^uint32(0)) {
		return 0, false
	}
	return uint32(v), true
}

// AsPriceFetcher is a tiny adapter that bridges the daemon's Cache into the
// `keeper.PriceFetcher` interface used by the ExtendVote handler.
//
// Returning the result of `Cache.Snapshot` keeps ExtendVote a pure read of
// in-process memory — there is never any sidecar I/O on the consensus
// goroutine.
func (d *Daemon) AsPriceFetcher() PriceFetcherFunc {
	return func(ctx context.Context, height int64) ([]MarketPrice, error) {
		_ = ctx
		_ = height
		return d.cache.Snapshot(time.Now().UTC(), d.cfg.MaxAge), nil
	}
}

// MarketPrice is re-exported so the public surface of the daemon does not
// require importing x/oracle/types. The keeper's PriceFetcher contract
// uses x/oracle/types.MarketPrice; the type alias below is provided in
// price_fetcher_adapter.go.
type MarketPrice = oracleMarketPrice

// PriceFetcherFunc mirrors keeper.PriceFetcherFunc but lives here so the
// daemon can hand back an adapter without circular-importing the keeper.
type PriceFetcherFunc func(ctx context.Context, height int64) ([]MarketPrice, error)

// FetchPrices implements the keeper.PriceFetcher interface.
func (f PriceFetcherFunc) FetchPrices(ctx context.Context, height int64) ([]MarketPrice, error) {
	return f(ctx, height)
}

// String returns a one-line summary of the daemon for log lines and
// metrics labels.
func (d *Daemon) String() string {
	return fmt.Sprintf("oracle-daemon[sidecar=%s interval=%s enabled=%t]",
		d.cfg.SidecarAddress, d.cfg.FetchInterval, d.cfg.Enabled)
}
