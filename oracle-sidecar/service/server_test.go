package service

import (
	"context"
	"log"
	"math/big"
	"net"
	"os"
	"sync"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"

	"github.com/perpdex/perpdex-l1/oracle-sidecar/oracle"
	"github.com/perpdex/perpdex-l1/oracle-sidecar/providers/types"
	pb "github.com/perpdex/perpdex-l1/oracle-sidecar/service/proto"
	"github.com/stretchr/testify/require"
)

// fakeProvider produces a fixed price for a fixed pair on a tick.
type fakeProvider struct {
	name  string
	pairs []types.CurrencyPair
	value *big.Int
	once  sync.Once
}

func (f *fakeProvider) Name() string                 { return f.name }
func (f *fakeProvider) Pairs() []types.CurrencyPair { return f.pairs }
func (f *fakeProvider) Start(ctx context.Context, out chan<- []types.Price) error {
	push := func() {
		batch := make([]types.Price, 0, len(f.pairs))
		for _, p := range f.pairs {
			batch = append(batch, types.Price{
				Pair:      p,
				Value:     new(big.Int).Set(f.value),
				Timestamp: time.Now().UTC(),
				Provider:  f.name,
			})
		}
		select {
		case out <- batch:
		case <-ctx.Done():
		}
	}
	push()
	tick := time.NewTicker(50 * time.Millisecond)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-tick.C:
			push()
		}
	}
}

func TestServerPricesEndToEnd(t *testing.T) {
	pair, _ := types.ParseCurrencyPair("BTC/USD")
	providers := []types.Provider{
		&fakeProvider{name: "p1", pairs: []types.CurrencyPair{pair}, value: big.NewInt(50000)},
		&fakeProvider{name: "p2", pairs: []types.CurrencyPair{pair}, value: big.NewInt(50100)},
		&fakeProvider{name: "p3", pairs: []types.CurrencyPair{pair}, value: big.NewInt(50200)},
	}
	logger := log.New(os.Stderr, "[test] ", 0)
	orch := oracle.New(logger, providers, oracle.AggregateConfig{
		MaxAge:     time.Second,
		MinSources: 2,
	})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	go func() { _ = orch.Run(ctx) }()

	lis, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	srv := NewServer(orch, BuildInfo{Version: "test"})
	go func() {
		gs := grpc.NewServer()
		pb.RegisterOracleServer(gs, srv)
		_ = gs.Serve(lis)
	}()

	conn, err := grpc.NewClient(lis.Addr().String(), grpc.WithTransportCredentials(insecure.NewCredentials()))
	require.NoError(t, err)
	defer conn.Close()
	client := pb.NewOracleClient(conn)

	deadline := time.After(2 * time.Second)
	for {
		select {
		case <-deadline:
			t.Fatalf("timed out waiting for snapshot")
		default:
		}
		resp, err := client.Prices(ctx, &pb.PricesRequest{})
		require.NoError(t, err)
		if v, ok := resp.Prices["BTC/USD"]; ok && v == "50100" {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
}
