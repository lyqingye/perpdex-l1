package keeper

import (
	"context"
	"errors"

	"cosmossdk.io/collections"
	"cosmossdk.io/math"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	perptypes "github.com/perpdex/perpdex-l1/types"
	"github.com/perpdex/perpdex-l1/x/funding/types"
)

type Querier struct{ k Keeper }

func NewQuerier(k Keeper) Querier { return Querier{k: k} }

var _ types.QueryServer = Querier{}

func (q Querier) Metadata(ctx context.Context, req *types.QueryMetadataRequest) (*types.QueryMetadataResponse, error) {
	if req == nil {
		return nil, status.Error(codes.InvalidArgument, "empty request")
	}
	m, err := q.k.Metadata.Get(ctx)
	if err != nil {
		return nil, status.Error(codes.Internal, err.Error())
	}
	return &types.QueryMetadataResponse{Metadata: m}, nil
}

func (q Querier) MarketFundingRate(ctx context.Context, req *types.QueryMarketFundingRateRequest) (*types.QueryMarketFundingRateResponse, error) {
	if req == nil {
		return nil, status.Error(codes.InvalidArgument, "empty request")
	}
	d, err := q.k.marketKeeper.GetMarketDetails(ctx, req.MarketIndex)
	if err != nil {
		if errors.Is(err, collections.ErrNotFound) {
			return nil, status.Error(codes.NotFound, err.Error())
		}
		return nil, status.Error(codes.Internal, err.Error())
	}
	m, err := q.k.Metadata.Get(ctx)
	if err != nil {
		return nil, status.Error(codes.Internal, err.Error())
	}
	return &types.QueryMarketFundingRateResponse{PrefixSum: d.FundingRatePrefixSum, LastSettledAt: m.LastFundingRoundTimestamp}, nil
}

func (q Querier) PositionPendingFunding(ctx context.Context, req *types.QueryPositionPendingFundingRequest) (*types.QueryPositionPendingFundingResponse, error) {
	if req == nil {
		return nil, status.Error(codes.InvalidArgument, "empty request")
	}
	pos, err := q.k.accountKeeper.GetPosition(ctx, req.AccountIndex, req.MarketIndex)
	if err != nil {
		if errors.Is(err, collections.ErrNotFound) {
			return nil, status.Error(codes.NotFound, err.Error())
		}
		return nil, status.Error(codes.Internal, err.Error())
	}
	d, err := q.k.marketKeeper.GetMarketDetails(ctx, req.MarketIndex)
	if err != nil {
		if errors.Is(err, collections.ErrNotFound) {
			return nil, status.Error(codes.NotFound, err.Error())
		}
		return nil, status.Error(codes.Internal, err.Error())
	}
	pending := pos.BaseSize.Mul(d.FundingRatePrefixSum.Sub(pos.LastFundingRatePrefixSum)).Quo(math.NewInt(perptypes.FundingRateTick))
	return &types.QueryPositionPendingFundingResponse{Pending: pending}, nil
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
