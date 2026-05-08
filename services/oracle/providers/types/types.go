// Package types defines the shared abstractions used by every concrete oracle
// price provider. Each provider runs in its own goroutine, periodically polls
// or subscribes to an upstream exchange, and pushes fresh prices into the
// supplied callback channel.
//
// The interface is deliberately minimal — the orchestrator in the `oracle`
// package is responsible for cross-provider median, staleness eviction and
// publishing snapshots over gRPC.
package types

import (
	"context"
	"fmt"
	"math/big"
	"strings"
	"time"
)

// CurrencyPair is the canonical identifier of a trading pair across providers
// and the chain side. It is an uppercase, slash-delimited token: "BTC/USD",
// "ETH/USD", "SOL/USDT".
type CurrencyPair struct {
	Base  string
	Quote string
}

// String returns the canonical "BASE/QUOTE" form.
func (c CurrencyPair) String() string {
	return fmt.Sprintf("%s/%s", c.Base, c.Quote)
}

// ParseCurrencyPair parses a "BASE/QUOTE" string. Whitespace is trimmed and
// both parts are upper-cased. An error is returned if the input does not have
// exactly one slash or either side is empty.
func ParseCurrencyPair(s string) (CurrencyPair, error) {
	s = strings.TrimSpace(s)
	parts := strings.SplitN(s, "/", 2)
	if len(parts) != 2 {
		return CurrencyPair{}, fmt.Errorf("currency pair %q must be of the form BASE/QUOTE", s)
	}
	base := strings.ToUpper(strings.TrimSpace(parts[0]))
	quote := strings.ToUpper(strings.TrimSpace(parts[1]))
	if base == "" || quote == "" {
		return CurrencyPair{}, fmt.Errorf("currency pair %q has empty base or quote", s)
	}
	return CurrencyPair{Base: base, Quote: quote}, nil
}

// Price is a single observation produced by a provider for a currency pair at
// a given timestamp. The value is kept as *big.Int to match the upstream
// Connect protocol; the chain side later truncates this into the
// market-specific fixed-point representation when it constructs the
// vote-extension payload.
type Price struct {
	Pair      CurrencyPair
	Value     *big.Int
	Timestamp time.Time
	// Provider is the canonical name of the source ("binance", "okx", ...).
	// Aggregators use this for outlier rejection and per-source dashboards.
	Provider string
}

// Provider is the contract every concrete exchange adapter implements.
type Provider interface {
	// Name is a stable, lower-case identifier used in logs/metrics.
	Name() string

	// Pairs returns the set of currency pairs this provider has been
	// configured to track for the current process. Implementations MUST
	// return the same slice across the lifetime of the provider.
	Pairs() []CurrencyPair

	// Start blocks until ctx is cancelled, periodically pushing fresh prices
	// onto out. Implementations are expected to be resilient to upstream
	// errors: a transient failure should not bring the goroutine down.
	Start(ctx context.Context, out chan<- []Price) error
}

// PriceFromFloat converts a float64 price into a *big.Int integer-encoded
// price using `decimals` decimal places. For example,
// PriceFromFloat(60123.45, 6) returns 60123450000.
//
// We do all maths in decimal form first (multiplying by 10**decimals) before
// converting to big.Int to avoid losing precision on very small or very large
// values; floating-point rounding is bounded to one ULP at the end.
func PriceFromFloat(value float64, decimals uint8) *big.Int {
	if value <= 0 {
		return big.NewInt(0)
	}
	mul := new(big.Float).SetInt(scale(decimals))
	bf := new(big.Float).SetFloat64(value)
	bf.Mul(bf, mul)
	out, _ := bf.Int(nil)
	if out == nil {
		return big.NewInt(0)
	}
	return out
}

// PriceFromString parses a decimal string ("60123.45") into an integer-encoded
// big.Int with `decimals` decimal places. Trailing zeros are tolerated.
func PriceFromString(s string, decimals uint8) (*big.Int, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return nil, fmt.Errorf("empty price string")
	}
	bf, _, err := big.ParseFloat(s, 10, 256, big.ToNearestEven)
	if err != nil {
		return nil, fmt.Errorf("parse price %q: %w", s, err)
	}
	if bf.Sign() <= 0 {
		return nil, fmt.Errorf("price %q must be positive", s)
	}
	bf.Mul(bf, new(big.Float).SetInt(scale(decimals)))
	out, _ := bf.Int(nil)
	if out == nil {
		return nil, fmt.Errorf("price %q is not finite", s)
	}
	return out, nil
}

func scale(decimals uint8) *big.Int {
	out := big.NewInt(1)
	ten := big.NewInt(10)
	for i := uint8(0); i < decimals; i++ {
		out.Mul(out, ten)
	}
	return out
}
