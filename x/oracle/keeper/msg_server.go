package keeper

import (
	"context"

	sdk "github.com/cosmos/cosmos-sdk/types"

	perptypes "github.com/perpdex/perpdex-l1/types"
	"github.com/perpdex/perpdex-l1/x/oracle/types"
)

type msgServer struct{ Keeper }

func NewMsgServerImpl(k Keeper) types.MsgServer { return &msgServer{Keeper: k} }

var _ types.MsgServer = msgServer{}

// AggregateOracleVotes is the on-chain landing of the aggregated price set.
//
// In a normal block this Msg is *injected* by the proposer's
// PrepareProposal handler as the first transaction (signed by the gov
// authority). The ante chain (`OracleInjectedTxDecorator`) rejects copies
// of this Msg coming from regular user transactions, so reaching this
// handler on the runtime path implies the VE pipeline produced it.
func (m msgServer) AggregateOracleVotes(ctx context.Context, msg *types.MsgAggregateOracleVotes) (*types.MsgAggregateOracleVotesResponse, error) {
	if msg.Authority != m.authority {
		return nil, types.ErrInvalidAuthority
	}
	now := sdk.UnwrapSDKContext(ctx).BlockTime().UnixMilli()
	for _, agg := range msg.Aggregations {
		p := types.OraclePrice{
			MarketIndex:          agg.MarketIndex,
			IndexPrice:           agg.IndexPrice,
			MarkPrice:            agg.MarkPrice,
			LastUpdatedTimestamp: now,
			LastUpdatedHeight:    msg.Height,
			ParticipantCount:     0,
		}
		// Apply the optional EMA smoothing on top of the freshly aggregated
		// mark price so that single-block spikes are dampened. The index
		// price is left untouched because risk / liquidation expect a raw
		// reference index.
		if err := m.applyMarkSmoothing(ctx, &p); err != nil {
			return nil, err
		}
		if err := m.SetPrice(ctx, p); err != nil {
			return nil, err
		}
	}
	_ = perptypes.OracleAggPosMedian // imported for future telemetry
	return &types.MsgAggregateOracleVotesResponse{}, nil
}

// applyMarkSmoothing applies a basis-points-encoded EMA on top of the new
// mark price. `alpha = 0` falls back to no smoothing; `alpha = 10000`
// means "fully trust the new sample". When no previous price exists the
// new sample is used verbatim.
func (m msgServer) applyMarkSmoothing(ctx context.Context, p *types.OraclePrice) error {
	params, err := m.Params.Get(ctx)
	if err != nil {
		return err
	}
	alpha := params.MarkPriceEmaAlpha
	if alpha == 0 || alpha >= 10_000 {
		return nil
	}
	prev, err := m.GetPrice(ctx, p.MarketIndex)
	if err != nil {
		// No prior price -> fall through with the raw sample.
		return nil
	}
	if prev.MarkPrice == 0 {
		return nil
	}
	// new = alpha * sample + (10000 - alpha) * prev, divided by 10000.
	// All inputs fit into uint64 comfortably for uint32 prices.
	smoothed := (uint64(alpha)*uint64(p.MarkPrice) + uint64(10_000-alpha)*uint64(prev.MarkPrice)) / 10_000
	if smoothed == 0 {
		smoothed = 1
	}
	p.MarkPrice = uint32(smoothed)
	return nil
}

func (m msgServer) UpdateParams(ctx context.Context, msg *types.MsgUpdateParams) (*types.MsgUpdateParamsResponse, error) {
	if msg.Authority != m.authority {
		return nil, types.ErrInvalidAuthority
	}
	if err := msg.Params.Validate(); err != nil {
		return nil, err
	}
	if err := m.Params.Set(ctx, msg.Params); err != nil {
		return nil, err
	}
	return &types.MsgUpdateParamsResponse{}, nil
}
