// Package okx implements an oracle price provider backed by OKX's public REST
// endpoint (GET /api/v5/market/tickers?instType=SPOT). One round-trip per tick
// is enough for the orchestrator's needs; see the binance package for the
// rationale behind preferring REST over WebSocket here.
package okx

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

const (
	DefaultEndpoint = "https://www.okx.com"
	DefaultInterval = 1500 * time.Millisecond
	DefaultTimeout  = 1200 * time.Millisecond
	DefaultDecimals = 8
)

type Config struct {
	Endpoint string
	Interval time.Duration
	Timeout  time.Duration
	Pairs    []types.CurrencyPair
	Decimals uint8
}

type Provider struct {
	cfg     Config
	http    *http.Client
	symbols map[string]types.CurrencyPair
}

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
		symbols[okxSymbol(pair)] = pair
	}
	return &Provider{
		cfg:     cfg,
		http:    &http.Client{Timeout: cfg.Timeout},
		symbols: symbols,
	}
}

func (p *Provider) Name() string                { return "okx" }
func (p *Provider) Pairs() []types.CurrencyPair { return p.cfg.Pairs }

func (p *Provider) Start(ctx context.Context, out chan<- []types.Price) error {
	ticker := time.NewTicker(p.cfg.Interval)
	defer ticker.Stop()

	if err := p.fetchAndPush(ctx, out); err != nil {
		fmt.Printf("[okx] initial fetch failed: %v\n", err)
	}
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
			if err := p.fetchAndPush(ctx, out); err != nil {
				fmt.Printf("[okx] fetch failed: %v\n", err)
			}
		}
	}
}

// envelope mirrors the JSON shape of GET /api/v5/market/tickers
type envelope struct {
	Code string        `json:"code"`
	Msg  string        `json:"msg"`
	Data []tickerEntry `json:"data"`
}

type tickerEntry struct {
	InstID string `json:"instId"` // e.g. "BTC-USDT"
	Last   string `json:"last"`
}

func (p *Provider) fetchAndPush(ctx context.Context, out chan<- []types.Price) error {
	url := strings.TrimRight(p.cfg.Endpoint, "/") + "/api/v5/market/tickers?instType=SPOT"
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

	var env envelope
	if err := json.NewDecoder(resp.Body).Decode(&env); err != nil {
		return fmt.Errorf("decode: %w", err)
	}
	if env.Code != "0" {
		return fmt.Errorf("okx api error: code=%s msg=%s", env.Code, env.Msg)
	}

	now := time.Now().UTC()
	prices := make([]types.Price, 0, len(p.cfg.Pairs))
	for _, e := range env.Data {
		pair, ok := p.symbols[e.InstID]
		if !ok {
			continue
		}
		val, err := types.PriceFromString(e.Last, p.cfg.Decimals)
		if err != nil {
			fmt.Printf("[okx] parse %s=%q failed: %v\n", e.InstID, e.Last, err)
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

// okxSymbol maps "BTC/USD" -> "BTC-USDT" because OKX only lists USDT spot for
// most majors. As with binance, the operator configures the canonical
// CurrencyPair once and we do the venue-specific renaming here.
func okxSymbol(pair types.CurrencyPair) string {
	quote := pair.Quote
	if quote == "USD" {
		quote = "USDT"
	}
	return strings.ToUpper(pair.Base) + "-" + strings.ToUpper(quote)
}
