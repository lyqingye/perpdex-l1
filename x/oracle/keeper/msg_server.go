package keeper

import (
	"context"

	"github.com/perpdex/perpdex-l1/x/oracle/types"
)

type msgServer struct{ Keeper }

// NewMsgServerImpl returns the gRPC msg server implementation. It only hosts
// `UpdateParams`; the on-chain price update path is now driven by the ABCI++
// pipeline (PreBlocker decodes the proposer-injected ExtendedCommitInfo and
// writes prices straight to state).
func NewMsgServerImpl(k Keeper) types.MsgServer { return &msgServer{Keeper: k} }

var _ types.MsgServer = msgServer{}

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
