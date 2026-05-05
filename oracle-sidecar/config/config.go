// Package config defines the on-disk JSON schema understood by the sidecar
// binary. The schema is intentionally narrow — we don't try to mirror every
// knob exposed by upstream Connect — because it is meant to be edited by hand
// in a `oracle.json` file and committed to ops repos.
package config

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"time"
)

// Config is the top-level shape of `oracle.json`.
//
// Example:
//
//	{
//	  "grpc_addr": ":8080",
//	  "metrics_addr": ":8002",
//	  "max_age": "5s",
//	  "min_sources": 1,
//	  "pairs": ["BTC/USD", "ETH/USD", "SOL/USD"],
//	  "providers": {
//	    "binance": { "enabled": true,  "interval": "1500ms" },
//	    "okx":     { "enabled": true,  "interval": "1500ms" },
//	    "coingecko": {
//	      "enabled": true,
//	      "interval": "5s",
//	      "slugs": { "BTC": "bitcoin", "ETH": "ethereum", "SOL": "solana" }
//	    }
//	  }
//	}
type Config struct {
	GRPCAddr    string                    `json:"grpc_addr"`
	MetricsAddr string                    `json:"metrics_addr"`
	MaxAge      Duration                  `json:"max_age"`
	MinSources  int                       `json:"min_sources"`
	Pairs       []string                  `json:"pairs"`
	Providers   map[string]ProviderConfig `json:"providers"`
}

// ProviderConfig is the per-provider sub-schema. Keys not understood by a
// specific adapter are ignored; this allows operators to share a single config
// shape across providers.
type ProviderConfig struct {
	Enabled  bool              `json:"enabled"`
	Endpoint string            `json:"endpoint"`
	APIKey   string            `json:"api_key"`
	Interval Duration          `json:"interval"`
	Timeout  Duration          `json:"timeout"`
	Decimals uint8             `json:"decimals"`
	Slugs    map[string]string `json:"slugs"` // CoinGecko-only
}

// Duration is a time.Duration that round-trips through JSON as a string ("5s").
type Duration time.Duration

func (d Duration) MarshalJSON() ([]byte, error) {
	return []byte(`"` + time.Duration(d).String() + `"`), nil
}

func (d *Duration) UnmarshalJSON(b []byte) error {
	s := strings.Trim(string(b), `"`)
	if s == "" || s == "null" {
		*d = 0
		return nil
	}
	v, err := time.ParseDuration(s)
	if err != nil {
		return fmt.Errorf("parse duration %q: %w", s, err)
	}
	*d = Duration(v)
	return nil
}

// Load reads and parses a Config from the given path. If path is empty a
// reasonable dev-stack default is returned.
func Load(path string) (*Config, error) {
	if path == "" {
		return Default(), nil
	}
	bz, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", path, err)
	}
	var cfg Config
	if err := json.Unmarshal(bz, &cfg); err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}
	cfg.applyDefaults()
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	return &cfg, nil
}

func (c *Config) applyDefaults() {
	if c.GRPCAddr == "" {
		c.GRPCAddr = ":8080"
	}
	if c.MetricsAddr == "" {
		c.MetricsAddr = ":8002"
	}
	if c.MaxAge == 0 {
		c.MaxAge = Duration(5 * time.Second)
	}
	if c.MinSources == 0 {
		c.MinSources = 1
	}
}

// Validate returns an error describing the first invariant violation found in
// the config.
func (c *Config) Validate() error {
	if len(c.Pairs) == 0 {
		return fmt.Errorf("config: at least one pair must be configured")
	}
	for _, p := range c.Pairs {
		if !strings.Contains(p, "/") {
			return fmt.Errorf("config: pair %q must be of the form BASE/QUOTE", p)
		}
	}
	enabled := 0
	for _, pc := range c.Providers {
		if pc.Enabled {
			enabled++
		}
	}
	if enabled == 0 {
		return fmt.Errorf("config: at least one provider must be enabled")
	}
	return nil
}

// Default returns an opinionated config sufficient to run a dev sidecar
// against public endpoints with no credentials.
func Default() *Config {
	c := &Config{
		Pairs: []string{"BTC/USD", "ETH/USD"},
		Providers: map[string]ProviderConfig{
			"binance": {Enabled: true, Interval: Duration(1500 * time.Millisecond), Decimals: 8},
			"okx":     {Enabled: true, Interval: Duration(1500 * time.Millisecond), Decimals: 8},
			"coingecko": {
				Enabled:  true,
				Interval: Duration(5 * time.Second),
				Decimals: 8,
				Slugs:    map[string]string{"BTC": "bitcoin", "ETH": "ethereum"},
			},
		},
	}
	c.applyDefaults()
	return c
}
