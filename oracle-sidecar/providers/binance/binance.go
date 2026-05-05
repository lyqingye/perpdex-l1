// Package binance implements an oracle price provider backed by Binance's
// public REST endpoint. We use REST (not WebSocket) because:
//   - the orchestrator only consumes one price per pair per tick anyway, so
//     ms-level latency is wasted;
//   - REST is dramatically simpler to operate (no reconnect logic, no per-pair
//     subscription book-keeping) and only adds a single HTTPS round-trip to
//     the per-tick latency budget.
//
// If you need higher fan-out, swap this file for a `gorilla/websocket` based
// implementation that keeps a persistent connection to wss://stream.binance.com.
package binance

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/perpdex/perpdex-l1/oracle-sidecar/providers/types"
)

// Default values used when the caller leaves Config zero-valued.
const (
	DefaultEndpoint   = "https://api.binance.com"
	DefaultInterval   = 1500 * time.Millisecond
	DefaultTimeout    = 1200 * time.Millisecond
	DefaultDecimals   = 8
	defaultMaxRetries = 2
)

// Config carries operator-supplied tunables. The zero value is intentionally
// usable: in that case the constants above apply.
type Config struct {
	Endpoint string
	Interval time.Duration
	Timeout  time.Duration
	// Pairs lists the currency pairs to fetch. Binance symbol mapping
	// (e.g. BTC/USD -> BTCUSDT) is handled internally; see
	// `binanceSymbol` below.
	Pairs []types.CurrencyPair
	// Decimals controls how many fractional digits are preserved when
	// converting Binance's decimal string into the integer encoding the
	// orchestrator expects. The chain re-truncates this to its on-chain
	// fixed-point precision later.
	Decimals uint8
}

// Provider is a thread-safe Binance REST adapter. It implements
// providers/types.Provider.
type Provider struct {
	cfg     Config
	http    *http.Client
	symbols map[string]types.CurrencyPair
}

// New constructs a Binance Provider from cfg, applying defaults for any
// zero-valued field.
func New(cfg Config) *Provider {
	if cfg.Endpoint == "" {
		cfg.Endpoint = DefaultEndpoint
	}
	if cfg.Interval == 0 {
		cfg.Interval = DefaultInterval
	}
	if cfg.Timeout == 0 {
		cfg.Timeout = DefaultTimeout
	}
	if cfg.Decimals == 0 {
		cfg.Decimals = DefaultDecimals
	}
	symbols := make(map[string]types.CurrencyPair, len(cfg.Pairs))
	for _, pair := range cfg.Pairs {
		symbols[binanceSymbol(pair)] = pair
	}
	return &Provider{
		cfg:     cfg,
		http:    &http.Client{Timeout: cfg.Timeout},
		symbols: symbols,
	}
}

// Name returns the canonical lower-case provider identifier.
func (p *Provider) Name() string { return "binance" }

// Pairs returns the configured currency pairs.
func (p *Provider) Pairs() []types.CurrencyPair { return p.cfg.Pairs }

// Start runs the polling loop until ctx is cancelled. Errors from a single
// poll are logged via the standard library and do not terminate the loop —
// upstream blips should not knock a provider out for the lifetime of the
// process.
func (p *Provider) Start(ctx context.Context, out chan<- []types.Price) error {
	ticker := time.NewTicker(p.cfg.Interval)
	defer ticker.Stop()

	if err := p.fetchAndPush(ctx, out); err != nil {
		// Log-and-continue: the first poll commonly races with daemon
		// startup or DNS warm-up.
		fmt.Printf("[binance] initial fetch failed: %v\n", err)
	}
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
			if err := p.fetchAndPush(ctx, out); err != nil {
				fmt.Printf("[binance] fetch failed: %v\n", err)
			}
		}
	}
}

// tickerEntry mirrors the JSON shape of GET /api/v3/ticker/price.
type tickerEntry struct {
	Symbol string `json:"symbol"`
	Price  string `json:"price"`
}

func (p *Provider) fetchAndPush(ctx context.Context, out chan<- []types.Price) error {
	url := strings.TrimRight(p.cfg.Endpoint, "/") + "/api/v3/ticker/price"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return fmt.Errorf("build request: %w", err)
	}
	resp, err := p.http.Do(req)
	if err != nil {
		return fmt.Errorf("http: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return fmt.Errorf("http %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}

	var entries []tickerEntry
	if err := json.NewDecoder(resp.Body).Decode(&entries); err != nil {
		return fmt.Errorf("decode: %w", err)
	}

	now := time.Now().UTC()
	prices := make([]types.Price, 0, len(p.cfg.Pairs))
	for _, e := range entries {
		pair, ok := p.symbols[e.Symbol]
		if !ok {
			continue
		}
		val, err := types.PriceFromString(e.Price, p.cfg.Decimals)
		if err != nil {
			fmt.Printf("[binance] parse %s=%q failed: %v\n", e.Symbol, e.Price, err)
			continue
		}
		prices = append(prices, types.Price{
			Pair:      pair,
			Value:     val,
			Timestamp: now,
			Provider:  p.Name(),
		})
	}

	select {
	case <-ctx.Done():
		return ctx.Err()
	case out <- prices:
	}
	return nil
}

// binanceSymbol maps a CurrencyPair to the Binance ticker symbol convention.
//
// Binance does not list direct USD pairs for every coin; the de-facto USD
// proxy is USDT. We rewrite the quote so callers can configure the canonical
// "BTC/USD" pair once and not care about the venue-specific quirk.
func binanceSymbol(pair types.CurrencyPair) string {
	quote := pair.Quote
	if quote == "USD" {
		quote = "USDT"
	}
	return strings.ToUpper(pair.Base + quote)
}
