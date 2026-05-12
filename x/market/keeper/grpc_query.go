package keeper

import (
	"context"
	"errors"

	"github.com/cosmos/cosmos-sdk/types/query"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/perpdex/perpdex-l1/x/market/types"
)

type Querier struct{ k Keeper }

func NewQuerier(k Keeper) Querier { return Querier{k: k} }

var _ types.QueryServer = Querier{}

// asGRPCError maps the keeper's domain errors onto gRPC codes so
// clients can tell "market doesn't exist" (NotFound) apart from
// "storage blew up" (Internal). Logic that does not match a known
// "not found" error is reported as Internal — keeping NotFound for
// expected misses only.
func asGRPCError(err error) error {
	if errors.Is(err, types.ErrMarketNotFound) {
		return status.Error(codes.NotFound, err.Error())
	}
	return status.Error(codes.Internal, err.Error())
}

func (q Querier) Market(ctx context.Context, req *types.QueryMarketRequest) (*types.QueryMarketResponse, error) {
	if req == nil {
		return nil, status.Error(codes.InvalidArgument, "empty request")
	}
	m, err := q.k.GetMarket(ctx, req.MarketIndex)
	if err != nil {
		return nil, asGRPCError(err)
	}
	return &types.QueryMarketResponse{Market: m}, nil
}

func (q Querier) MarketDetails(ctx context.Context, req *types.QueryMarketDetailsRequest) (*types.QueryMarketDetailsResponse, error) {
	if req == nil {
		return nil, status.Error(codes.InvalidArgument, "empty request")
	}
	d, err := q.k.GetMarketDetails(ctx, req.MarketIndex)
	if err != nil {
		return nil, asGRPCError(err)
	}
	return &types.QueryMarketDetailsResponse{Details: d}, nil
}

// Markets returns the (optionally filtered, paginated) list of
// markets. Filtering by market_type is opt-in via `FilterByType`
// because proto3 cannot distinguish an unset `MarketType` from
// `MarketTypePerps == 0`; without the explicit flag a request for
// "only perps" would be observationally identical to "any market".
//
// The pagination envelope is honoured by CollectionFilteredPaginate
// so clients get correct next-page tokens, total counts, etc.
func (q Querier) Markets(ctx context.Context, req *types.QueryMarketsRequest) (*types.QueryMarketsResponse, error) {
	if req == nil {
		return nil, status.Error(codes.InvalidArgument, "empty request")
	}
	out, pageRes, err := query.CollectionFilteredPaginate(
		ctx, q.k.Markets, req.Pagination,
		func(_ uint32, v types.Market) (bool, error) {
			if req.FilterByType && v.MarketType != req.MarketType {
				return false, nil
			}
			return true, nil
		},
		func(_ uint32, v types.Market) (types.Market, error) { return v, nil },
	)
	if err != nil {
		return nil, status.Error(codes.Internal, err.Error())
	}
	if out == nil {
		out = []types.Market{}
	}
	return &types.QueryMarketsResponse{Markets: out, Pagination: pageRes}, nil
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
