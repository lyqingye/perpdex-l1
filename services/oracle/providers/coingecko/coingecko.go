// Package coingecko implements an oracle price provider backed by the
// CoinGecko Pro/Free /simple/price API. CoinGecko is intentionally chosen as
// the third source because:
//   - it covers a far wider asset universe than centralised exchanges;
//   - its rate limits (10-30 req/min on free tier) match the orchestrator's
//     polling cadence well;
//   - it does not require any API key for spot price reads on majors, easing
//     dev-stack bootstrap.
//
// In production you SHOULD set Config.APIKey to a Pro key and bump
// Config.Interval to whatever your plan allows.
package coingecko

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/perpdex/perpdex-l1/oracle-sidecar/providers/types"
)

const (
	DefaultEndpoint = "https://api.coingecko.com"
	DefaultInterval = 5 * time.Second
	DefaultTimeout  = 3 * time.Second
	DefaultDecimals = 8
)

type Config struct {
	Endpoint string
	APIKey   string
	Interval time.Duration
	Timeout  time.Duration
	Pairs    []types.CurrencyPair
	Decimals uint8
	// Slugs maps the CurrencyPair base symbol to the CoinGecko coin id (e.g.
	// "BTC" -> "bitcoin", "ETH" -> "ethereum"). Operators are required to
	// supply this because CoinGecko has no global symbol/id index.
	Slugs map[string]string
}

type Provider struct {
	cfg    Config
	http   *http.Client
	idToCP map[string]types.CurrencyPair
	idsCSV string
	vsCCY  string
}

func New(cfg Config) (*Provider, error) {
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
	if len(cfg.Slugs) == 0 {
		return nil, fmt.Errorf("coingecko: cfg.Slugs must map base symbols to coin ids")
	}
	idToCP := make(map[string]types.CurrencyPair, len(cfg.Pairs))
	ids := make([]string, 0, len(cfg.Pairs))
	vsCCY := ""
	for _, pair := range cfg.Pairs {
		id, ok := cfg.Slugs[pair.Base]
		if !ok {
			return nil, fmt.Errorf("coingecko: missing slug for %q (Slugs config)", pair.Base)
		}
		idToCP[id] = pair
		ids = append(ids, id)
		// All pairs in a single batch must share the same vs currency.
		// Most callers use USD; we enforce that here for simplicity.
		quote := strings.ToLower(pair.Quote)
		if quote == "usdt" {
			quote = "usd"
		}
		if vsCCY == "" {
			vsCCY = quote
		} else if vsCCY != quote {
			return nil, fmt.Errorf("coingecko: all pairs must share the same quote (got %s and %s)", vsCCY, quote)
		}
	}
	if vsCCY == "" {
		vsCCY = "usd"
	}
	return &Provider{
		cfg:    cfg,
		http:   &http.Client{Timeout: cfg.Timeout},
		idToCP: idToCP,
		idsCSV: strings.Join(ids, ","),
		vsCCY:  vsCCY,
	}, nil
}

func (p *Provider) Name() string                { return "coingecko" }
func (p *Provider) Pairs() []types.CurrencyPair { return p.cfg.Pairs }

func (p *Provider) Start(ctx context.Context, out chan<- []types.Price) error {
	ticker := time.NewTicker(p.cfg.Interval)
	defer ticker.Stop()

	if err := p.fetchAndPush(ctx, out); err != nil {
		fmt.Printf("[coingecko] initial fetch failed: %v\n", err)
	}
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
			if err := p.fetchAndPush(ctx, out); err != nil {
				fmt.Printf("[coingecko] fetch failed: %v\n", err)
			}
		}
	}
}

func (p *Provider) fetchAndPush(ctx context.Context, out chan<- []types.Price) error {
	q := url.Values{}
	q.Set("ids", p.idsCSV)
	q.Set("vs_currencies", p.vsCCY)
	endpoint := strings.TrimRight(p.cfg.Endpoint, "/") + "/api/v3/simple/price?" + q.Encode()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return fmt.Errorf("build request: %w", err)
	}
	if p.cfg.APIKey != "" {
		req.Header.Set("x-cg-pro-api-key", p.cfg.APIKey)
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

	// CoinGecko returns: {"bitcoin":{"usd":61234.5}, "ethereum":{"usd":3210.1}}
	var raw map[string]map[string]json.RawMessage
	if err := json.NewDecoder(resp.Body).Decode(&raw); err != nil {
		return fmt.Errorf("decode: %w", err)
	}

	now := time.Now().UTC()
	prices := make([]types.Price, 0, len(p.cfg.Pairs))
	for id, m := range raw {
		pair, ok := p.idToCP[id]
		if !ok {
			continue
		}
		valBytes, ok := m[p.vsCCY]
		if !ok {
			continue
		}
		// CoinGecko serializes the value as a JSON number; allow either
		// number or quoted string.
		valStr := string(valBytes)
		valStr = strings.Trim(valStr, "\"")
		if _, err := strconv.ParseFloat(valStr, 64); err != nil {
			fmt.Printf("[coingecko] parse %s=%q failed: %v\n", id, valStr, err)
			continue
		}
		val, err := types.PriceFromString(valStr, p.cfg.Decimals)
		if err != nil {
			fmt.Printf("[coingecko] convert %s=%q failed: %v\n", id, valStr, err)
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
