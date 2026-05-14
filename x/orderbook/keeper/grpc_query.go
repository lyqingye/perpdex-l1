package keeper

import (
	"context"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"cosmossdk.io/collections"
	"github.com/cosmos/cosmos-sdk/types/query"

	"github.com/perpdex/perpdex-l1/x/orderbook/types"
)

// maxOrderBookSnapshotDepth caps the per-side levels returned by the
// snapshot RPC when the caller passes `depth = 0` (or a value above
// the cap). The cap protects the RPC from accidental full-book pulls
// that would dominate validator query CPU on busy markets.
const maxOrderBookSnapshotDepth = uint32(50)

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
func (q Querier) Orders(ctx context.Context, req *types.QueryOrdersRequest) (*types.QueryOrdersResponse, error) {
	if req == nil {
		return nil, status.Error(codes.InvalidArgument, "empty request")
	}
	out, pageRes, err := query.CollectionPaginate(
		ctx,
		q.k.Orders,
		req.Pagination,
		func(_ uint64, o types.Order) (types.Order, error) {
			return o, nil
		},
	)
	if err != nil {
		return nil, status.Error(codes.Internal, err.Error())
	}
	filtered := out[:0]
	for _, o := range out {
		if req.AccountIndex != 0 && o.OwnerAccountIndex != req.AccountIndex {
			continue
		}
		if req.MarketIndex != 0 && o.MarketIndex != req.MarketIndex {
			continue
		}
		filtered = append(filtered, o)
	}
	return &types.QueryOrdersResponse{Orders: filtered, Pagination: pageRes}, nil
}

// OrderBookSnapshot honours `req.Depth` and limits the iteration to the
// requested market via a prefix range. A zero / over-cap depth is
// clamped to `maxOrderBookSnapshotDepth` to avoid full-book dumps.
func (q Querier) OrderBookSnapshot(ctx context.Context, req *types.QueryOrderBookSnapshotRequest) (*types.QueryOrderBookSnapshotResponse, error) {
	if req == nil {
		return nil, status.Error(codes.InvalidArgument, "empty request")
	}
	depth := req.Depth
	if depth == 0 || depth > maxOrderBookSnapshotDepth {
		depth = maxOrderBookSnapshotDepth
	}
	rng := collections.NewPrefixedPairRange[uint32, uint32](req.MarketIndex)
	iter, err := q.k.PriceLevels.Iterate(ctx, rng)
	if err != nil {
		return nil, status.Error(codes.Internal, err.Error())
	}
	defer iter.Close()
	out := make([]types.PriceLevelAggregate, 0, depth)
	for ; iter.Valid() && uint32(len(out)) < depth; iter.Next() {
		v, err := iter.Value()
		if err != nil {
			return nil, status.Error(codes.Internal, err.Error())
		}
		out = append(out, v)
	}
	return &types.QueryOrderBookSnapshotResponse{Levels: out}, nil
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
