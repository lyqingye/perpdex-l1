// Package service exposes the Oracle gRPC service that the perpdex-l1 chain
// daemon polls every block to retrieve the latest aggregated prices.
package service

import (
	"context"
	"fmt"
	"net"

	"google.golang.org/grpc"

	"github.com/perpdex/perpdex-l1/oracle-sidecar/oracle"
	pb "github.com/perpdex/perpdex-l1/oracle-sidecar/service/proto"
)

// Server implements the sidecarpb.OracleServer interface backed by an
// Orchestrator's snapshot.
type Server struct {
	pb.UnimplementedOracleServer

	orch    *oracle.Orchestrator
	version BuildInfo
}

// BuildInfo carries the build metadata returned by the Version RPC.
type BuildInfo struct {
	Version   string
	GitCommit string
	BuildDate string
}

// NewServer wires an Orchestrator into the gRPC handler.
func NewServer(orch *oracle.Orchestrator, info BuildInfo) *Server {
	if info.Version == "" {
		info.Version = "v0.0.0-dev"
	}
	return &Server{orch: orch, version: info}
}

// Prices implements sidecarpb.OracleServer/Prices. It performs no upstream
// I/O: it reads from the orchestrator's in-memory snapshot only.
func (s *Server) Prices(ctx context.Context, _ *pb.PricesRequest) (*pb.PricesResponse, error) {
	snap, ts := s.orch.Snapshot()
	resp := &pb.PricesResponse{
		Prices:    make(map[string]string, len(snap)),
		Timestamp: ts.UnixNano(),
	}
	for k, v := range snap {
		resp.Prices[k] = v.String()
	}
	return resp, nil
}

// Version implements sidecarpb.OracleServer/Version.
func (s *Server) Version(_ context.Context, _ *pb.VersionRequest) (*pb.VersionResponse, error) {
	return &pb.VersionResponse{
		Version:   s.version.Version,
		GitCommit: s.version.GitCommit,
		BuildDate: s.version.BuildDate,
	}, nil
}

// Serve starts a gRPC server bound to addr (e.g. ":8080") and blocks until
// ctx is cancelled.
func Serve(ctx context.Context, addr string, srv *Server) error {
	lis, err := net.Listen("tcp", addr)
	if err != nil {
		return fmt.Errorf("listen %s: %w", addr, err)
	}
	gs := grpc.NewServer()
	pb.RegisterOracleServer(gs, srv)

	go func() {
		<-ctx.Done()
		gs.GracefulStop()
	}()

	if err := gs.Serve(lis); err != nil {
		return fmt.Errorf("grpc serve: %w", err)
	}
	return nil
}
