package keeper

import (
	"context"
	"errors"

	"cosmossdk.io/collections"

	sdk "github.com/cosmos/cosmos-sdk/types"

	perptypes "github.com/perpdex/perpdex-l1/types"
	"github.com/perpdex/perpdex-l1/x/oracle/types"
)

type msgServer struct{ Keeper }

func NewMsgServerImpl(k Keeper) types.MsgServer { return &msgServer{Keeper: k} }

var _ types.MsgServer = msgServer{}

func (m msgServer) BindOracleOperator(ctx context.Context, msg *types.MsgBindOracleOperator) (*types.MsgBindOracleOperatorResponse, error) {
	// Authority: validator operator address (signer must be the operator).
	if msg.Sender != msg.ValidatorAddress {
		return nil, types.ErrUnauthorized
	}
	if exists, err := m.Bindings.Has(ctx, msg.ValidatorAddress); err != nil {
		return nil, err
	} else if exists {
		return nil, types.ErrBindingExists
	}
	now := sdk.UnwrapSDKContext(ctx).BlockTime().UnixMilli()
	height := sdk.UnwrapSDKContext(ctx).BlockHeight()
	binding := types.ValidatorOracleBinding{
		ValidatorAddress:      msg.ValidatorAddress,
		OracleOperatorAddress: msg.OracleOperatorAddress,
		BoundAtBlock:          height,
		BoundAtTime:           now,
		Metadata:              msg.Metadata,
	}
	if err := m.Bindings.Set(ctx, msg.ValidatorAddress, binding); err != nil {
		return nil, err
	}
	if err := m.OperatorIdx.Set(ctx, msg.OracleOperatorAddress, msg.ValidatorAddress); err != nil {
		return nil, err
	}
	return &types.MsgBindOracleOperatorResponse{}, nil
}

func (m msgServer) UnbindOracleOperator(ctx context.Context, msg *types.MsgUnbindOracleOperator) (*types.MsgUnbindOracleOperatorResponse, error) {
	binding, err := m.Bindings.Get(ctx, msg.ValidatorAddress)
	if err != nil {
		if errors.Is(err, collections.ErrNotFound) {
			return nil, types.ErrBindingNotFound
		}
		return nil, err
	}
	if msg.Sender != msg.ValidatorAddress {
		return nil, types.ErrUnauthorized
	}
	_ = m.OperatorIdx.Remove(ctx, binding.OracleOperatorAddress)
	if err := m.Bindings.Remove(ctx, msg.ValidatorAddress); err != nil {
		return nil, err
	}
	return &types.MsgUnbindOracleOperatorResponse{}, nil
}

// AggregateOracleVotes is invoked by the proposer (via the chain authority)
// after vote-extension aggregation. It applies the aggregated prices to the
// oracle store.
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
			AggregationMethod:    perptypes.OracleAggPosMedian,
			ParticipantCount:     uint32(len(msg.VoterRecords)),
		}
		if err := m.SetPrice(ctx, p); err != nil {
			return nil, err
		}
	}
	for _, vr := range msg.VoterRecords {
		s, err := m.Stats.Get(ctx, vr.ValidatorAddress)
		if err != nil && !errors.Is(err, collections.ErrNotFound) {
			return nil, err
		}
		if errors.Is(err, collections.ErrNotFound) {
			s = types.ValidatorOracleStats{ValidatorAddress: vr.ValidatorAddress}
		}
		if vr.Participated {
			s.TotalVotesSubmitted++
			s.LastActiveHeight = msg.Height
			s.ConsecutiveMissed = 0
		} else {
			s.TotalVotesMissed++
			s.ConsecutiveMissed++
		}
		s.TotalVotesDeviant += uint64(vr.DeviantMarketCount)
		if err := m.Stats.Set(ctx, vr.ValidatorAddress, s); err != nil {
			return nil, err
		}
	}
	return &types.MsgAggregateOracleVotesResponse{}, nil
}

// InjectOracle is the WHITELIST mode entrypoint. The signer must be a
// registered oracle provider.
func (m msgServer) InjectOracle(ctx context.Context, msg *types.MsgInjectOracle) (*types.MsgInjectOracleResponse, error) {
	prov, err := m.Providers.Get(ctx, msg.Sender)
	if err != nil {
		if errors.Is(err, collections.ErrNotFound) {
			return nil, types.ErrUnauthorized
		}
		return nil, err
	}
	if !prov.Enabled {
		return nil, types.ErrProviderDisabled
	}
	params, err := m.Params.Get(ctx)
	if err != nil {
		return nil, err
	}
	if params.AggregationMode != perptypes.OracleAggWhitelist {
		return nil, types.ErrInvalidMode.Wrap("inject only allowed in WHITELIST mode")
	}
	now := sdk.UnwrapSDKContext(ctx).BlockTime().UnixMilli()
	height := sdk.UnwrapSDKContext(ctx).BlockHeight()
	for _, mp := range msg.Prices {
		op := types.OraclePrice{
			MarketIndex:          mp.MarketIndex,
			IndexPrice:           mp.IndexPrice,
			MarkPrice:            mp.MarkPrice,
			LastUpdatedTimestamp: now,
			LastUpdatedHeight:    height,
			AggregationMethod:    perptypes.OracleAggWhitelist,
			ParticipantCount:     1,
		}
		if err := m.SetPrice(ctx, op); err != nil {
			return nil, err
		}
	}
	return &types.MsgInjectOracleResponse{}, nil
}

func (m msgServer) AddOracleProvider(ctx context.Context, msg *types.MsgAddOracleProvider) (*types.MsgAddOracleProviderResponse, error) {
	if msg.Authority != m.authority {
		return nil, types.ErrInvalidAuthority
	}
	now := sdk.UnwrapSDKContext(ctx).BlockTime().UnixMilli()
	prov := types.OracleProvider{Address: msg.Address, Enabled: true, AddedAt: now, Description: msg.Description}
	if err := m.Providers.Set(ctx, msg.Address, prov); err != nil {
		return nil, err
	}
	return &types.MsgAddOracleProviderResponse{}, nil
}

func (m msgServer) UpdateOracleProvider(ctx context.Context, msg *types.MsgUpdateOracleProvider) (*types.MsgUpdateOracleProviderResponse, error) {
	if msg.Authority != m.authority {
		return nil, types.ErrInvalidAuthority
	}
	prov, err := m.Providers.Get(ctx, msg.Address)
	if err != nil {
		if errors.Is(err, collections.ErrNotFound) {
			return nil, types.ErrProviderNotFound
		}
		return nil, err
	}
	prov.Enabled = msg.Enabled
	if err := m.Providers.Set(ctx, msg.Address, prov); err != nil {
		return nil, err
	}
	return &types.MsgUpdateOracleProviderResponse{}, nil
}

func (m msgServer) SetAggregationMode(ctx context.Context, msg *types.MsgSetAggregationMode) (*types.MsgSetAggregationModeResponse, error) {
	if msg.Authority != m.authority {
		return nil, types.ErrInvalidAuthority
	}
	if msg.NewMode != perptypes.OracleAggPosMedian && msg.NewMode != perptypes.OracleAggWhitelist {
		return nil, types.ErrInvalidMode
	}
	p, err := m.Params.Get(ctx)
	if err != nil {
		return nil, err
	}
	p.AggregationMode = msg.NewMode
	if err := m.Params.Set(ctx, p); err != nil {
		return nil, err
	}
	return &types.MsgSetAggregationModeResponse{}, nil
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
