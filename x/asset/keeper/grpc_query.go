package keeper

import (
	"context"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/perpdex/perpdex-l1/x/asset/types"
)

// Querier wraps Keeper to provide gRPC query handlers without colliding with
// the keeper's collection field names (Assets, Params).
type Querier struct {
	k Keeper
}

func NewQuerier(k Keeper) Querier { return Querier{k: k} }

var _ types.QueryServer = Querier{}

func (q Querier) Asset(ctx context.Context, req *types.QueryAssetRequest) (*types.QueryAssetResponse, error) {
	if req == nil {
		return nil, status.Error(codes.InvalidArgument, "empty request")
	}
	a, err := q.k.GetAsset(ctx, req.AssetIndex)
	if err != nil {
		return nil, status.Error(codes.NotFound, err.Error())
	}
	return &types.QueryAssetResponse{Asset: a}, nil
}

func (q Querier) AssetByDenom(ctx context.Context, req *types.QueryAssetByDenomRequest) (*types.QueryAssetByDenomResponse, error) {
	if req == nil {
		return nil, status.Error(codes.InvalidArgument, "empty request")
	}
	a, err := q.k.GetAssetByDenom(ctx, req.Denom)
	if err != nil {
		return nil, status.Error(codes.NotFound, err.Error())
	}
	return &types.QueryAssetByDenomResponse{Asset: a}, nil
}

func (q Querier) Assets(ctx context.Context, _ *types.QueryAssetsRequest) (*types.QueryAssetsResponse, error) {
	out, err := q.k.AllAssets(ctx)
	if err != nil {
		return nil, status.Error(codes.Internal, err.Error())
	}
	return &types.QueryAssetsResponse{Assets: out}, nil
}

func (q Querier) Params(ctx context.Context, _ *types.QueryParamsRequest) (*types.QueryParamsResponse, error) {
	p, err := q.k.Params.Get(ctx)
	if err != nil {
		return nil, status.Error(codes.Internal, err.Error())
	}
	return &types.QueryParamsResponse{Params: p}, nil
}
