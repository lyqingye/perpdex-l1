package keeper

import (
	"context"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/perpdex/perpdex-l1/x/oracle/types"
)

type Querier struct{ k Keeper }

func NewQuerier(k Keeper) Querier { return Querier{k: k} }

var _ types.QueryServer = Querier{}

func (q Querier) OraclePrice(ctx context.Context, req *types.QueryOraclePriceRequest) (*types.QueryOraclePriceResponse, error) {
	if req == nil {
		return nil, status.Error(codes.InvalidArgument, "empty request")
	}
	p, err := q.k.GetPrice(ctx, req.MarketIndex)
	if err != nil {
		return nil, status.Error(codes.NotFound, err.Error())
	}
	return &types.QueryOraclePriceResponse{Price: p}, nil
}

func (q Querier) OracleProviders(ctx context.Context, _ *types.QueryOracleProvidersRequest) (*types.QueryOracleProvidersResponse, error) {
	out, err := q.k.AllProviders(ctx)
	if err != nil {
		return nil, status.Error(codes.Internal, err.Error())
	}
	return &types.QueryOracleProvidersResponse{Providers: out}, nil
}

func (q Querier) Bindings(ctx context.Context, _ *types.QueryBindingsRequest) (*types.QueryBindingsResponse, error) {
	out, err := q.k.AllBindings(ctx)
	if err != nil {
		return nil, status.Error(codes.Internal, err.Error())
	}
	return &types.QueryBindingsResponse{Bindings: out}, nil
}

func (q Querier) Params(ctx context.Context, _ *types.QueryParamsRequest) (*types.QueryParamsResponse, error) {
	p, err := q.k.Params.Get(ctx)
	if err != nil {
		return nil, status.Error(codes.Internal, err.Error())
	}
	return &types.QueryParamsResponse{Params: p}, nil
}
