package keeper

import (
	"context"

	"cosmossdk.io/collections"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/perpdex/perpdex-l1/x/account/types"
)

type Querier struct{ k Keeper }

func NewQuerier(k Keeper) Querier { return Querier{k: k} }

var _ types.QueryServer = Querier{}

func (q Querier) Account(ctx context.Context, req *types.QueryAccountRequest) (*types.QueryAccountResponse, error) {
	if req == nil {
		return nil, status.Error(codes.InvalidArgument, "empty request")
	}
	a, err := q.k.GetAccount(ctx, req.AccountIndex)
	if err != nil {
		return nil, status.Error(codes.NotFound, err.Error())
	}
	return &types.QueryAccountResponse{Account: a}, nil
}

func (q Querier) AccountByOwner(ctx context.Context, req *types.QueryAccountByOwnerRequest) (*types.QueryAccountByOwnerResponse, error) {
	if req == nil {
		return nil, status.Error(codes.InvalidArgument, "empty request")
	}
	a, err := q.k.GetMasterAccountByOwner(ctx, req.OwnerAddress)
	if err != nil {
		return nil, status.Error(codes.NotFound, err.Error())
	}
	return &types.QueryAccountByOwnerResponse{Account: a}, nil
}

func (q Querier) SubAccounts(ctx context.Context, req *types.QuerySubAccountsRequest) (*types.QuerySubAccountsResponse, error) {
	if req == nil {
		return nil, status.Error(codes.InvalidArgument, "empty request")
	}
	out := []types.Account{}
	if err := q.k.IterateAccounts(ctx, func(a types.Account) bool {
		if a.MasterAccountIndex == req.MasterAccountIndex {
			out = append(out, a)
		}
		return false
	}); err != nil {
		return nil, status.Error(codes.Internal, err.Error())
	}
	return &types.QuerySubAccountsResponse{Accounts: out}, nil
}

func (q Querier) AccountAssets(ctx context.Context, req *types.QueryAccountAssetsRequest) (*types.QueryAccountAssetsResponse, error) {
	if req == nil {
		return nil, status.Error(codes.InvalidArgument, "empty request")
	}
	pref := collections.NewPrefixedPairRange[uint64, uint32](req.AccountIndex)
	iter, err := q.k.AccountAssets.Iterate(ctx, pref)
	if err != nil {
		return nil, status.Error(codes.Internal, err.Error())
	}
	defer iter.Close()
	out := []types.AccountAsset{}
	for ; iter.Valid(); iter.Next() {
		v, err := iter.Value()
		if err != nil {
			return nil, status.Error(codes.Internal, err.Error())
		}
		out = append(out, v)
	}
	return &types.QueryAccountAssetsResponse{Assets: out}, nil
}

func (q Querier) AccountPositions(ctx context.Context, req *types.QueryAccountPositionsRequest) (*types.QueryAccountPositionsResponse, error) {
	if req == nil {
		return nil, status.Error(codes.InvalidArgument, "empty request")
	}
	pref := collections.NewPrefixedPairRange[uint64, uint32](req.AccountIndex)
	iter, err := q.k.AccountPositions.Iterate(ctx, pref)
	if err != nil {
		return nil, status.Error(codes.Internal, err.Error())
	}
	defer iter.Close()
	out := []types.AccountPosition{}
	for ; iter.Valid(); iter.Next() {
		v, err := iter.Value()
		if err != nil {
			return nil, status.Error(codes.Internal, err.Error())
		}
		out = append(out, v)
	}
	return &types.QueryAccountPositionsResponse{Positions: out}, nil
}

func (q Querier) AccountPosition(ctx context.Context, req *types.QueryAccountPositionRequest) (*types.QueryAccountPositionResponse, error) {
	if req == nil {
		return nil, status.Error(codes.InvalidArgument, "empty request")
	}
	p, err := q.k.GetPosition(ctx, req.AccountIndex, req.MarketIndex)
	if err != nil {
		return nil, status.Error(codes.Internal, err.Error())
	}
	return &types.QueryAccountPositionResponse{Position: p}, nil
}

func (q Querier) Params(ctx context.Context, _ *types.QueryParamsRequest) (*types.QueryParamsResponse, error) {
	p, err := q.k.Params.Get(ctx)
	if err != nil {
		return nil, status.Error(codes.Internal, err.Error())
	}
	return &types.QueryParamsResponse{Params: p}, nil
}
