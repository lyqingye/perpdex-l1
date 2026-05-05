package daemon

import (
	"context"
	"fmt"
	"math/big"
	"strings"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"

	sidecarpb "github.com/perpdex/perpdex-l1/oracle-sidecar/service/proto"
)

// SidecarClient wraps the gRPC connection to the local oracle-sidecar. It is
// intentionally minimal: a single Prices call at a configurable interval and
// a Version probe used at startup to log compatibility warnings.
//
// All calls carry a per-request timeout; the daemon goroutine is fully
// responsible for retry/backoff. We never block consensus on a sidecar
// round-trip — the daemon writes through to a local Cache that the ABCI
// handlers read.
type SidecarClient struct {
	conn   *grpc.ClientConn
	client sidecarpb.OracleClient

	addr    string
	timeout time.Duration
}

// ClientConfig is supplied at construction time.
type ClientConfig struct {
	// Address is the dial-string of the local sidecar (e.g. "localhost:8080").
	Address string
	// Timeout caps a single Prices call. Defaults to 200ms.
	Timeout time.Duration
}

// NewSidecarClient dials the sidecar and returns a ready client. The caller
// MUST invoke Close when done. Dial uses insecure credentials by default
// because the sidecar is expected to live on the same host as the chain
// process; operators that cross hosts should swap in TLS.
func NewSidecarClient(cfg ClientConfig) (*SidecarClient, error) {
	if cfg.Address == "" {
		cfg.Address = "localhost:8080"
	}
	if cfg.Timeout <= 0 {
		cfg.Timeout = 200 * time.Millisecond
	}
	conn, err := grpc.NewClient(
		cfg.Address,
		grpc.WithTransportCredentials(insecure.NewCredentials()),
	)
	if err != nil {
		return nil, fmt.Errorf("oracle daemon: dial %s: %w", cfg.Address, err)
	}
	return &SidecarClient{
		conn:    conn,
		client:  sidecarpb.NewOracleClient(conn),
		addr:    cfg.Address,
		timeout: cfg.Timeout,
	}, nil
}

// Close tears down the gRPC connection.
func (c *SidecarClient) Close() error {
	if c.conn == nil {
		return nil
	}
	return c.conn.Close()
}

// Address returns the dial string the client was configured with.
func (c *SidecarClient) Address() string { return c.addr }

// PriceQuote is what FetchPrices returns: the canonical pair (e.g. "BTC/USD")
// and the integer-encoded price expressed in sidecar precision (typically 8
// decimals). Callers are responsible for re-scaling into chain precision.
type PriceQuote struct {
	Pair  string
	Value *big.Int
}

// FetchPrices issues a single Prices RPC and returns the parsed quotes. An
// error is returned for any failure — the daemon decides whether to back off
// or simply skip a tick.
func (c *SidecarClient) FetchPrices(ctx context.Context) ([]PriceQuote, error) {
	if c.client == nil {
		return nil, fmt.Errorf("oracle daemon: client not initialised")
	}
	cctx, cancel := context.WithTimeout(ctx, c.timeout)
	defer cancel()

	resp, err := c.client.Prices(cctx, &sidecarpb.PricesRequest{})
	if err != nil {
		return nil, fmt.Errorf("oracle daemon: Prices RPC: %w", err)
	}
	if resp == nil {
		return nil, fmt.Errorf("oracle daemon: nil response from sidecar")
	}
	out := make([]PriceQuote, 0, len(resp.Prices))
	for k, v := range resp.Prices {
		v = strings.TrimSpace(v)
		if v == "" {
			continue
		}
		bi, ok := new(big.Int).SetString(v, 10)
		if !ok {
			continue
		}
		if bi.Sign() <= 0 {
			continue
		}
		out = append(out, PriceQuote{Pair: k, Value: bi})
	}
	return out, nil
}

// ProbeVersion calls Version once and returns the sidecar's reported version
// string. Used at startup to log a compatibility line. It deliberately does
// not error: an absent sidecar is not a fatal condition for the chain process.
func (c *SidecarClient) ProbeVersion(ctx context.Context) string {
	if c.client == nil {
		return ""
	}
	cctx, cancel := context.WithTimeout(ctx, c.timeout)
	defer cancel()
	resp, err := c.client.Version(cctx, &sidecarpb.VersionRequest{})
	if err != nil || resp == nil {
		return ""
	}
	return resp.Version
}
