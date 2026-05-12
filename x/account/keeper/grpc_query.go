package keeper

import (
	"context"

	"cosmossdk.io/collections"

	"github.com/cosmos/cosmos-sdk/types/query"

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
	// Walk only the master's prefix on the (masterIdx, subIdx) keyset
	// rather than scanning every account, then fan each sub-index out
	// to the canonical Account row. Pagination uses the keyset's own
	// store so the response is bounded.
	out, pageRes, err := query.CollectionPaginate(
		ctx, q.k.MasterSubAccounts, req.Pagination,
		func(key collections.Pair[uint64, uint64], _ collections.NoValue) (types.Account, error) {
			return q.k.GetAccount(ctx, key.K2())
		},
		query.WithCollectionPaginationPairPrefix[uint64, uint64](req.MasterAccountIndex),
	)
	if err != nil {
		return nil, status.Error(codes.Internal, err.Error())
	}
	if out == nil {
		out = []types.Account{}
	}
	return &types.QuerySubAccountsResponse{Accounts: out, Pagination: pageRes}, nil
}

func (q Querier) AccountAssets(ctx context.Context, req *types.QueryAccountAssetsRequest) (*types.QueryAccountAssetsResponse, error) {
	if req == nil {
		return nil, status.Error(codes.InvalidArgument, "empty request")
	}
	out, pageRes, err := query.CollectionPaginate(
		ctx, q.k.AccountAssets, req.Pagination,
		func(_ collections.Pair[uint64, uint32], v types.AccountAsset) (types.AccountAsset, error) {
			v.NormalizeIntFields()
			return v, nil
		},
		query.WithCollectionPaginationPairPrefix[uint64, uint32](req.AccountIndex),
	)
	if err != nil {
		return nil, status.Error(codes.Internal, err.Error())
	}
	if out == nil {
		out = []types.AccountAsset{}
	}
	return &types.QueryAccountAssetsResponse{Assets: out, Pagination: pageRes}, nil
}

func (q Querier) AccountPositions(ctx context.Context, req *types.QueryAccountPositionsRequest) (*types.QueryAccountPositionsResponse, error) {
	if req == nil {
		return nil, status.Error(codes.InvalidArgument, "empty request")
	}
	out, pageRes, err := query.CollectionPaginate(
		ctx, q.k.AccountPositions, req.Pagination,
		func(_ collections.Pair[uint64, uint32], v types.AccountPosition) (types.AccountPosition, error) {
			v.NormalizeIntFields()
			return v, nil
		},
		query.WithCollectionPaginationPairPrefix[uint64, uint32](req.AccountIndex),
	)
	if err != nil {
		return nil, status.Error(codes.Internal, err.Error())
	}
	if out == nil {
		out = []types.AccountPosition{}
	}
	return &types.QueryAccountPositionsResponse{Positions: out, Pagination: pageRes}, nil
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

func (q Querier) PublicPoolInfo(ctx context.Context, req *types.QueryPublicPoolInfoRequest) (*types.QueryPublicPoolInfoResponse, error) {
	if req == nil {
		return nil, status.Error(codes.InvalidArgument, "empty request")
	}
	a, err := q.k.GetAccount(ctx, req.AccountIndex)
	if err != nil {
		return nil, status.Error(codes.NotFound, err.Error())
	}
	if a.PublicPoolInfo == nil {
		return nil, status.Errorf(codes.NotFound, "account %d is not a public pool", req.AccountIndex)
	}
	return &types.QueryPublicPoolInfoResponse{Info: *a.PublicPoolInfo}, nil
}

func (q Querier) PublicPoolShares(ctx context.Context, req *types.QueryPublicPoolSharesRequest) (*types.QueryPublicPoolSharesResponse, error) {
	if req == nil {
		return nil, status.Error(codes.InvalidArgument, "empty request")
	}
	a, err := q.k.GetAccount(ctx, req.AccountIndex)
	if err != nil {
		return nil, status.Error(codes.NotFound, err.Error())
	}
	return &types.QueryPublicPoolSharesResponse{Shares: a.PublicPoolShares}, nil
}

func (q Querier) SharesToUSDCValue(ctx context.Context, req *types.QuerySharesToUSDCValueRequest) (*types.QuerySharesToUSDCValueResponse, error) {
	if req == nil {
		return nil, status.Error(codes.InvalidArgument, "empty request")
	}
	usdc, err := q.k.SharesToUSDCValue(ctx, req.PoolAccountIndex, req.ShareAmount)
	if err != nil {
		return nil, status.Error(codes.Internal, err.Error())
	}
	return &types.QuerySharesToUSDCValueResponse{UsdcAmount: usdc}, nil
}
