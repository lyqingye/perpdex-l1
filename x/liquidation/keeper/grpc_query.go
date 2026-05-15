package keeper

import (
	"context"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/perpdex/perpdex-l1/x/liquidation/types"
)

type Querier struct{ k Keeper }

func NewQuerier(k Keeper) Querier { return Querier{k: k} }

var _ types.QueryServer = Querier{}

func (q Querier) Params(ctx context.Context, _ *types.QueryParamsRequest) (*types.QueryParamsResponse, error) {
	p, err := q.k.Params.Get(ctx)
	if err != nil {
		return nil, status.Error(codes.Internal, err.Error())
	}
	return &types.QueryParamsResponse{Params: p}, nil
}

func (q Querier) ADLQueue(ctx context.Context, req *types.QueryADLQueueRequest) (*types.QueryADLQueueResponse, error) {
	if req == nil {
		return nil, status.Error(codes.InvalidArgument, "empty request")
	}
	limit := req.Limit
	if limit == 0 {
		params, err := q.k.Params.Get(ctx)
		if err != nil {
			return nil, status.Error(codes.Internal, err.Error())
		}
		limit = params.MaxAdlCandidatesPerVictim
		if limit == 0 {
			limit = types.DefaultMaxADLCandidatesPerVictim
		}
	}
	cands, err := q.k.BuildADLQueue(ctx, req.MarketIndex, req.OppositeIsLong, limit)
	if err != nil {
		return nil, status.Error(codes.Internal, err.Error())
	}
	out := make([]types.ADLCandidate, 0, len(cands))
	for _, c := range cands {
		out = append(out, types.ADLCandidate{
			AccountIndex:  c.AccountIndex,
			PositionSize:  c.PositionSize,
			UnrealizedPnl: c.UnrealizedPnL,
			Score:         c.Score,
		})
	}
	return &types.QueryADLQueueResponse{Candidates: out}, nil
}
