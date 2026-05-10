package keeper

import (
	"context"

	"cosmossdk.io/math"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	perptypes "github.com/perpdex/perpdex-l1/types"
	"github.com/perpdex/perpdex-l1/x/funding/types"
)

type Querier struct{ k Keeper }

func NewQuerier(k Keeper) Querier { return Querier{k: k} }

var _ types.QueryServer = Querier{}

func (q Querier) Metadata(ctx context.Context, _ *types.QueryMetadataRequest) (*types.QueryMetadataResponse, error) {
	m, err := q.k.Metadata.Get(ctx)
	if err != nil {
		return nil, status.Error(codes.Internal, err.Error())
	}
	return &types.QueryMetadataResponse{Metadata: m}, nil
}

func (q Querier) MarketFundingRate(ctx context.Context, req *types.QueryMarketFundingRateRequest) (*types.QueryMarketFundingRateResponse, error) {
	d, err := q.k.marketKeeper.GetMarketDetails(ctx, req.MarketIndex)
	if err != nil {
		return nil, status.Error(codes.NotFound, err.Error())
	}
	m, err := q.k.Metadata.Get(ctx)
	if err != nil {
		return nil, status.Error(codes.Internal, err.Error())
	}
	return &types.QueryMarketFundingRateResponse{PrefixSum: d.FundingRatePrefixSum, LastSettledAt: m.LastFundingRoundTimestamp}, nil
}

func (q Querier) PositionPendingFunding(ctx context.Context, req *types.QueryPositionPendingFundingRequest) (*types.QueryPositionPendingFundingResponse, error) {
	pos, err := q.k.accountKeeper.GetPosition(ctx, req.AccountIndex, req.MarketIndex)
	if err != nil {
		return nil, status.Error(codes.NotFound, err.Error())
	}
	d, err := q.k.marketKeeper.GetMarketDetails(ctx, req.MarketIndex)
	if err != nil {
		return nil, status.Error(codes.NotFound, err.Error())
	}
	pending := pos.Size_.Mul(d.FundingRatePrefixSum.Sub(pos.LastFundingRatePrefixSum)).Quo(math.NewInt(perptypes.FundingRateTick))
	return &types.QueryPositionPendingFundingResponse{Pending: pending}, nil
}

func (q Querier) Params(ctx context.Context, _ *types.QueryParamsRequest) (*types.QueryParamsResponse, error) {
	p, err := q.k.Params.Get(ctx)
	if err != nil {
		return nil, status.Error(codes.Internal, err.Error())
	}
	return &types.QueryParamsResponse{Params: p}, nil
}
