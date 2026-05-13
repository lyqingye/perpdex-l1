package keeper

import (
	"context"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/perpdex/perpdex-l1/x/orderbook/types"
)

type Querier struct{ k Keeper }

func NewQuerier(k Keeper) Querier { return Querier{k: k} }

var _ types.QueryServer = Querier{}

func (q Querier) Order(ctx context.Context, req *types.QueryOrderRequest) (*types.QueryOrderResponse, error) {
	o, err := q.k.GetOrder(ctx, req.OrderIndex)
	if err != nil {
		return nil, status.Error(codes.NotFound, err.Error())
	}
	return &types.QueryOrderResponse{Order: o}, nil
}

func (q Querier) OrderByClientId(ctx context.Context, req *types.QueryOrderByClientIdRequest) (*types.QueryOrderByClientIdResponse, error) {
	o, err := q.k.GetOrderByClientID(ctx, req.MarketIndex, req.AccountIndex, req.ClientOrderIndex)
	if err != nil {
		return nil, status.Error(codes.NotFound, err.Error())
	}
	return &types.QueryOrderByClientIdResponse{Order: o}, nil
}

func (q Querier) Orders(ctx context.Context, _ *types.QueryOrdersRequest) (*types.QueryOrdersResponse, error) {
	out := []types.Order{}
	iter, err := q.k.Orders.Iterate(ctx, nil)
	if err != nil {
		return nil, status.Error(codes.Internal, err.Error())
	}
	defer iter.Close()
	for ; iter.Valid(); iter.Next() {
		v, err := iter.Value()
		if err != nil {
			return nil, status.Error(codes.Internal, err.Error())
		}
		out = append(out, v)
	}
	return &types.QueryOrdersResponse{Orders: out}, nil
}

func (q Querier) OrderBookSnapshot(ctx context.Context, req *types.QueryOrderBookSnapshotRequest) (*types.QueryOrderBookSnapshotResponse, error) {
	out := []types.PriceLevelAggregate{}
	iter, err := q.k.PriceLevels.Iterate(ctx, nil)
	if err != nil {
		return nil, status.Error(codes.Internal, err.Error())
	}
	defer iter.Close()
	for ; iter.Valid(); iter.Next() {
		v, err := iter.Value()
		if err != nil {
			return nil, status.Error(codes.Internal, err.Error())
		}
		if v.MarketIndex == req.MarketIndex {
			out = append(out, v)
		}
	}
	return &types.QueryOrderBookSnapshotResponse{Levels: out}, nil
}

func (q Querier) BestBidAsk(ctx context.Context, req *types.QueryBestBidAskRequest) (*types.QueryBestBidAskResponse, error) {
	bid, ask, err := q.k.BestBidAsk(ctx, req.MarketIndex)
	if err != nil {
		return nil, status.Error(codes.Internal, err.Error())
	}
	return &types.QueryBestBidAskResponse{BestBid: bid, BestAsk: ask}, nil
}

func (q Querier) ImpactPrice(ctx context.Context, req *types.QueryImpactPriceRequest) (*types.QueryImpactPriceResponse, error) {
	bidImp, bidOk, err := q.k.ComputeImpactPrice(ctx, req.MarketIndex, false)
	if err != nil {
		return nil, status.Error(codes.Internal, err.Error())
	}
	askImp, askOk, err := q.k.ComputeImpactPrice(ctx, req.MarketIndex, true)
	if err != nil {
		return nil, status.Error(codes.Internal, err.Error())
	}
	// Mid is only meaningful when both sides resolved. Returning a
	// half-zero mid when one side has no depth would silently halve
	// the price and corrupt any consumer that uses it as a mark
	// proxy. Both flags are surfaced so callers can detect the
	// degenerate case explicitly.
	var mid uint32
	if bidOk && askOk {
		mid = uint32((uint64(bidImp) + uint64(askImp)) / 2)
	}
	return &types.QueryImpactPriceResponse{
		ImpactBid:   bidImp,
		ImpactAsk:   askImp,
		ImpactPrice: mid,
		BidOk:       bidOk,
		AskOk:       askOk,
	}, nil
}

func (q Querier) Params(ctx context.Context, _ *types.QueryParamsRequest) (*types.QueryParamsResponse, error) {
	p, err := q.k.Params.Get(ctx)
	if err != nil {
		return nil, status.Error(codes.Internal, err.Error())
	}
	return &types.QueryParamsResponse{Params: p}, nil
}
