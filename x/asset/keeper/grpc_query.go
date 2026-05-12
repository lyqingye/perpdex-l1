package keeper

import (
	"context"
	"errors"

	"github.com/cosmos/cosmos-sdk/types/query"

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

// asGRPCError maps domain errors onto the right gRPC status code so
// monitors / clients can tell "asset doesn't exist" apart from "storage
// blew up".
func asGRPCError(err error) error {
	if errors.Is(err, types.ErrAssetNotFound) {
		return status.Error(codes.NotFound, err.Error())
	}
	return status.Error(codes.Internal, err.Error())
}

func (q Querier) Asset(ctx context.Context, req *types.QueryAssetRequest) (*types.QueryAssetResponse, error) {
	if req == nil {
		return nil, status.Error(codes.InvalidArgument, "empty request")
	}
	a, err := q.k.GetAsset(ctx, req.AssetIndex)
	if err != nil {
		return nil, asGRPCError(err)
	}
	return &types.QueryAssetResponse{Asset: a}, nil
}

func (q Querier) AssetByDenom(ctx context.Context, req *types.QueryAssetByDenomRequest) (*types.QueryAssetByDenomResponse, error) {
	if req == nil {
		return nil, status.Error(codes.InvalidArgument, "empty request")
	}
	a, err := q.k.GetAssetByDenom(ctx, req.Denom)
	if err != nil {
		return nil, asGRPCError(err)
	}
	return &types.QueryAssetByDenomResponse{Asset: a}, nil
}

func (q Querier) Assets(ctx context.Context, req *types.QueryAssetsRequest) (*types.QueryAssetsResponse, error) {
	if req == nil {
		return nil, status.Error(codes.InvalidArgument, "empty request")
	}
	out, pageRes, err := query.CollectionPaginate(
		ctx, q.k.Assets, req.Pagination,
		func(_ uint32, v types.Asset) (types.Asset, error) { return v, nil },
	)
	if err != nil {
		return nil, status.Error(codes.Internal, err.Error())
	}
	if out == nil {
		out = []types.Asset{}
	}
	return &types.QueryAssetsResponse{Assets: out, Pagination: pageRes}, nil
}

func (q Querier) Params(ctx context.Context, req *types.QueryParamsRequest) (*types.QueryParamsResponse, error) {
	if req == nil {
		return nil, status.Error(codes.InvalidArgument, "empty request")
	}
	p, err := q.k.Params.Get(ctx)
	if err != nil {
		return nil, status.Error(codes.Internal, err.Error())
	}
	return &types.QueryParamsResponse{Params: p}, nil
}
