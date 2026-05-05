package daemon_test

import (
	"context"
	"net"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"

	sidecarpb "github.com/perpdex/perpdex-l1/oracle-sidecar/service/proto"
	"github.com/perpdex/perpdex-l1/x/oracle/daemon"
)

// fakeSidecar implements the OracleServer interface with a configurable
// snapshot. Used by client tests to drive the bufconn end-to-end.
type fakeSidecar struct {
	sidecarpb.UnimplementedOracleServer
	snapshot map[string]string
}

func (f *fakeSidecar) Prices(_ context.Context, _ *sidecarpb.PricesRequest) (*sidecarpb.PricesResponse, error) {
	return &sidecarpb.PricesResponse{Prices: f.snapshot, Timestamp: time.Now().UnixNano()}, nil
}

func (f *fakeSidecar) Version(_ context.Context, _ *sidecarpb.VersionRequest) (*sidecarpb.VersionResponse, error) {
	return &sidecarpb.VersionResponse{Version: "fake"}, nil
}

func startFakeSidecar(t *testing.T, snapshot map[string]string) (string, func()) {
	t.Helper()
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	gs := grpc.NewServer()
	sidecarpb.RegisterOracleServer(gs, &fakeSidecar{snapshot: snapshot})
	go func() { _ = gs.Serve(lis) }()
	return lis.Addr().String(), func() { gs.GracefulStop() }
}

func TestSidecarClientFetchPrices(t *testing.T) {
	addr, stop := startFakeSidecar(t, map[string]string{
		"BTC/USD": "6000000000000",
		"ETH/USD": "350000000000",
	})
	defer stop()

	c, err := daemon.NewSidecarClient(daemon.ClientConfig{Address: addr, Timeout: 500 * time.Millisecond})
	require.NoError(t, err)
	defer c.Close()

	got, err := c.FetchPrices(context.Background())
	require.NoError(t, err)
	require.Len(t, got, 2)
	pairs := map[string]string{}
	for _, q := range got {
		pairs[q.Pair] = q.Value.String()
	}
	require.Equal(t, "6000000000000", pairs["BTC/USD"])
	require.Equal(t, "350000000000", pairs["ETH/USD"])
}

func TestSidecarClientProbeVersion(t *testing.T) {
	addr, stop := startFakeSidecar(t, map[string]string{})
	defer stop()
	c, err := daemon.NewSidecarClient(daemon.ClientConfig{Address: addr, Timeout: 500 * time.Millisecond})
	require.NoError(t, err)
	defer c.Close()
	require.Equal(t, "fake", c.ProbeVersion(context.Background()))
}

func TestSidecarClientTimeout(t *testing.T) {
	// dial a non-listening port; FetchPrices should fail within Timeout.
	c, err := daemon.NewSidecarClient(daemon.ClientConfig{
		Address: "127.0.0.1:1",
		Timeout: 50 * time.Millisecond,
	})
	require.NoError(t, err)
	defer c.Close()
	start := time.Now()
	_, err = c.FetchPrices(context.Background())
	require.Error(t, err)
	require.Less(t, time.Since(start), time.Second)
}

// keep the unused insecure import alive: gRPC dial in NewSidecarClient
// already uses it; reference here so go vet doesn't trim the import.
var _ = insecure.NewCredentials
