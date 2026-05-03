package keeper

import (
	"context"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/perpdex/perpdex-l1/x/risk/types"
)

type Querier struct{ k Keeper }

func NewQuerier(k Keeper) Querier { return Querier{k: k} }

var _ types.QueryServer = Querier{}

func (q Querier) RiskInfo(ctx context.Context, req *types.QueryRiskInfoRequest) (*types.QueryRiskInfoResponse, error) {
	ri, err := q.k.ComputeRiskInfo(ctx, req.AccountIndex)
	if err != nil {
		return nil, status.Error(codes.Internal, err.Error())
	}
	return &types.QueryRiskInfoResponse{RiskInfo: ri}, nil
}

func (q Querier) IsolatedRisk(ctx context.Context, req *types.QueryIsolatedRiskRequest) (*types.QueryIsolatedRiskResponse, error) {
	rp, err := q.k.ComputeIsolatedRisk(ctx, req.AccountIndex, req.MarketIndex)
	if err != nil {
		return nil, status.Error(codes.Internal, err.Error())
	}
	return &types.QueryIsolatedRiskResponse{Risk: rp}, nil
}

func (q Querier) HealthStatus(ctx context.Context, req *types.QueryHealthStatusRequest) (*types.QueryHealthStatusResponse, error) {
	s, err := q.k.GetHealthStatus(ctx, req.AccountIndex)
	if err != nil {
		return nil, status.Error(codes.Internal, err.Error())
	}
	return &types.QueryHealthStatusResponse{Status: s}, nil
}

func (q Querier) AvailableCollateral(ctx context.Context, req *types.QueryAvailableCollateralRequest) (*types.QueryAvailableCollateralResponse, error) {
	v, err := q.k.GetAvailableCollateral(ctx, req.AccountIndex)
	if err != nil {
		return nil, status.Error(codes.Internal, err.Error())
	}
	return &types.QueryAvailableCollateralResponse{Available: v}, nil
}

func (q Querier) PositionZeroPrice(ctx context.Context, req *types.QueryPositionZeroPriceRequest) (*types.QueryPositionZeroPriceResponse, error) {
	p, err := q.k.GetPositionZeroPrice(ctx, req.AccountIndex, req.MarketIndex)
	if err != nil {
		return nil, status.Error(codes.Internal, err.Error())
	}
	return &types.QueryPositionZeroPriceResponse{ZeroPrice: p}, nil
}
