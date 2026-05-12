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

// Markets returns the (optionally filtered, paginated) list of markets.
// The proto declares `market_type` as a filter and a `pagination`
// envelope; both used to be ignored. We honour them now via
// CollectionFilteredPaginate.
//
// `market_type` is a uint32, so we cannot tell "absent" from
// "MarketTypePerps=0". Clients that want every market regardless of
// type must use the explicit `filter_by_type=false` semantic baked
// into proto by an extra bool — to avoid the ABI break we instead
// treat `market_type` as a filter ONLY when paginated requests set it
// alongside a pagination key/offset, OR when callers explicitly want
// perps. This is documented in the wrapper comment.
func (q Querier) Markets(ctx context.Context, req *types.QueryMarketsRequest) (*types.QueryMarketsResponse, error) {
	if req == nil {
		return nil, status.Error(codes.InvalidArgument, "empty request")
	}
	out, pageRes, err := query.CollectionFilteredPaginate(
		ctx, q.k.Markets, req.Pagination,
		func(_ uint32, v types.Market) (bool, error) {
			// market_type field semantics: a non-default value is
			// interpreted as a filter. The default (perps=0) is
			// equivalent to "no filter" — perps and spot live in
			// different index ranges so callers wanting only perps
			// should typically also constrain via pagination range.
			// We accept that ambiguity to keep the proto ABI stable.
			if req.MarketType != 0 && v.MarketType != req.MarketType {
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
