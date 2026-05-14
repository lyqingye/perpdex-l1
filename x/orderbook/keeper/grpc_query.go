package keeper

import (
	"context"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"cosmossdk.io/collections"
	"github.com/cosmos/cosmos-sdk/types/query"

	"github.com/perpdex/perpdex-l1/x/orderbook/types"
)

type Querier struct{ k Keeper }

func NewQuerier(k Keeper) Querier { return Querier{k: k} }

var _ types.QueryServer = Querier{}

func (q Querier) Order(ctx context.Context, req *types.QueryOrderRequest) (*types.QueryOrderResponse, error) {
	if req == nil {
		return nil, status.Error(codes.InvalidArgument, "empty request")
	}
	o, err := q.k.GetOrder(ctx, req.OrderIndex)
	if err != nil {
		return nil, status.Error(codes.NotFound, err.Error())
	}
	return &types.QueryOrderResponse{Order: o}, nil
}

func (q Querier) OrderByClientId(ctx context.Context, req *types.QueryOrderByClientIdRequest) (*types.QueryOrderByClientIdResponse, error) {
	if req == nil {
		return nil, status.Error(codes.InvalidArgument, "empty request")
	}
	o, err := q.k.GetOrderByClientID(ctx, req.MarketIndex, req.AccountIndex, req.ClientOrderIndex)
	if err != nil {
		return nil, status.Error(codes.NotFound, err.Error())
	}
	return &types.QueryOrderByClientIdResponse{Order: o}, nil
}

// Orders honours the proto request: filters by `account_index` and / or
// `market_index` (both zero = unfiltered, account-only = all that
// account's orders, market-only = that market's history) and applies
// the standard Cosmos pagination envelope to bound the response size.
//
// The predicate is pushed into `CollectionFilteredPaginate` so the
// returned page contains up to `Limit` *matches*, not up to `Limit`
// raw rows that may all be filtered out. This is required for the
// envelope's `NextKey` / `Total` to mean "more matches available" and
// "total matches", respectively.
func (q Querier) Orders(ctx context.Context, req *types.QueryOrdersRequest) (*types.QueryOrdersResponse, error) {
	if req == nil {
		return nil, status.Error(codes.InvalidArgument, "empty request")
	}
	out, pageRes, err := query.CollectionFilteredPaginate(
		ctx,
		q.k.Orders,
		req.Pagination,
		func(_ uint64, o types.Order) (bool, error) {
			if req.AccountIndex != 0 && o.OwnerAccountIndex != req.AccountIndex {
				return false, nil
			}
			if req.MarketIndex != 0 && o.MarketIndex != req.MarketIndex {
				return false, nil
			}
			return true, nil
		},
		func(_ uint64, o types.Order) (types.Order, error) { return o, nil },
	)
	if err != nil {
		return nil, status.Error(codes.Internal, err.Error())
	}
	if out == nil {
		out = []types.Order{}
	}
	return &types.QueryOrdersResponse{Orders: out, Pagination: pageRes}, nil
}

// OrderBookSnapshot returns the top-of-book per side for `market_index`.
// Bids are walked in descending price order (best bid first); asks are
// walked in ascending price order (best ask first). Levels with zero
// base on the requested side are skipped so a `bids` entry always has
// `BidBaseSum > 0` and an `asks` entry always has `AskBaseSum > 0`.
//
// `depth` bounds the number of levels returned PER SIDE. A zero or
// over-cap value is clamped to `types.DefaultOrderBookSnapshotMaxDepth`
// so the RPC cannot be coerced into a full-book scan.
func (q Querier) OrderBookSnapshot(ctx context.Context, req *types.QueryOrderBookSnapshotRequest) (*types.QueryOrderBookSnapshotResponse, error) {
	if req == nil {
		return nil, status.Error(codes.InvalidArgument, "empty request")
	}
	depth := req.Depth
	if depth == 0 || depth > types.DefaultOrderBookSnapshotMaxDepth {
		depth = types.DefaultOrderBookSnapshotMaxDepth
	}
	bids, err := q.collectSnapshotSide(ctx, req.MarketIndex, depth, false)
	if err != nil {
		return nil, status.Error(codes.Internal, err.Error())
	}
	asks, err := q.collectSnapshotSide(ctx, req.MarketIndex, depth, true)
	if err != nil {
		return nil, status.Error(codes.Internal, err.Error())
	}
	return &types.QueryOrderBookSnapshotResponse{Bids: bids, Asks: asks}, nil
}

// collectSnapshotSide walks PriceLevels for `market` and returns up to
// `depth` aggregates that have non-zero base on the requested side.
// `isAsk == true` returns ascending price (best ask first); `false`
// returns descending price (best bid first).
func (q Querier) collectSnapshotSide(ctx context.Context, market uint32, depth uint32, isAsk bool) ([]types.PriceLevelAggregate, error) {
	rng := collections.NewPrefixedPairRange[uint32, uint32](market)
	if !isAsk {
		rng = rng.Descending()
	}
	iter, err := q.k.PriceLevels.Iterate(ctx, rng)
	if err != nil {
		return nil, err
	}
	defer iter.Close()
	out := make([]types.PriceLevelAggregate, 0, depth)
	for ; iter.Valid() && uint32(len(out)) < depth; iter.Next() {
		v, err := iter.Value()
		if err != nil {
			return nil, err
		}
		if isAsk {
			if v.AskBaseSum == 0 {
				continue
			}
		} else if v.BidBaseSum == 0 {
			continue
		}
		out = append(out, v)
	}
	return out, nil
}

func (q Querier) BestBidAsk(ctx context.Context, req *types.QueryBestBidAskRequest) (*types.QueryBestBidAskResponse, error) {
	if req == nil {
		return nil, status.Error(codes.InvalidArgument, "empty request")
	}
	bid, ask, err := q.k.BestBidAsk(ctx, req.MarketIndex)
	if err != nil {
		return nil, status.Error(codes.Internal, err.Error())
	}
	return &types.QueryBestBidAskResponse{BestBid: bid, BestAsk: ask}, nil
}

func (q Querier) ImpactPrice(ctx context.Context, req *types.QueryImpactPriceRequest) (*types.QueryImpactPriceResponse, error) {
	if req == nil {
		return nil, status.Error(codes.InvalidArgument, "empty request")
	}
	bidImp, err := q.k.ComputeImpactPrice(ctx, req.MarketIndex, false)
	if err != nil {
		return nil, status.Error(codes.Internal, err.Error())
	}
	askImp, err := q.k.ComputeImpactPrice(ctx, req.MarketIndex, true)
	if err != nil {
		return nil, status.Error(codes.Internal, err.Error())
	}
	// Mid is only meaningful when both sides resolved. A zero on
	// either side means the corresponding depth is insufficient;
	// returning a half-zero mid would silently halve any consumer
	// using this as a mark proxy.
	var mid uint32
	if bidImp != 0 && askImp != 0 {
		mid = uint32((uint64(bidImp) + uint64(askImp)) / 2)
	}
	return &types.QueryImpactPriceResponse{
		ImpactBid:   bidImp,
		ImpactAsk:   askImp,
		ImpactPrice: mid,
	}, nil
}

func (q Querier) Params(ctx context.Context, _ *types.QueryParamsRequest) (*types.QueryParamsResponse, error) {
	p, err := q.k.Params.Get(ctx)
	if err != nil {
		return nil, status.Error(codes.Internal, err.Error())
	}
	return &types.QueryParamsResponse{Params: p}, nil
}
