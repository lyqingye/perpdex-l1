package keeper

import (
	"context"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/perpdex/perpdex-l1/x/market/types"
)

type Querier struct{ k Keeper }

func NewQuerier(k Keeper) Querier { return Querier{k: k} }

var _ types.QueryServer = Querier{}

func (q Querier) Market(ctx context.Context, req *types.QueryMarketRequest) (*types.QueryMarketResponse, error) {
	if req == nil {
		return nil, status.Error(codes.InvalidArgument, "empty request")
	}
	m, err := q.k.GetMarket(ctx, req.MarketIndex)
	if err != nil {
		return nil, status.Error(codes.NotFound, err.Error())
	}
	return &types.QueryMarketResponse{Market: m}, nil
}

func (q Querier) MarketDetails(ctx context.Context, req *types.QueryMarketDetailsRequest) (*types.QueryMarketDetailsResponse, error) {
	if req == nil {
		return nil, status.Error(codes.InvalidArgument, "empty request")
	}
	d, err := q.k.GetMarketDetails(ctx, req.MarketIndex)
	if err != nil {
		return nil, status.Error(codes.NotFound, err.Error())
	}
	return &types.QueryMarketDetailsResponse{Details: d}, nil
}

func (q Querier) Markets(ctx context.Context, _ *types.QueryMarketsRequest) (*types.QueryMarketsResponse, error) {
	out := []types.Market{}
	if err := q.k.IterateMarkets(ctx, func(m types.Market) bool { out = append(out, m); return false }); err != nil {
		return nil, status.Error(codes.Internal, err.Error())
	}
	return &types.QueryMarketsResponse{Markets: out}, nil
}

func (q Querier) Params(ctx context.Context, _ *types.QueryParamsRequest) (*types.QueryParamsResponse, error) {
	p, err := q.k.Params.Get(ctx)
	if err != nil {
		return nil, status.Error(codes.Internal, err.Error())
	}
	return &types.QueryParamsResponse{Params: p}, nil
}
