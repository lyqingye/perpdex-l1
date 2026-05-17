package keeper

import (
	"context"

	"github.com/perpdex/perpdex-l1/x/liquidation/types"
)

type msgServer struct{ Keeper }

func NewMsgServerImpl(k Keeper) types.MsgServer { return &msgServer{Keeper: k} }

var _ types.MsgServer = msgServer{}

func (m msgServer) Liquidate(ctx context.Context, msg *types.MsgLiquidate) (*types.MsgLiquidateResponse, error) {
	if err := msg.ValidateBasic(); err != nil {
		return nil, err
	}
	// Partial liquidation has no counterparty (fills against the
	// public book) and is permissionless — anyone can poke an
	// underwater account; LLP/IF collects the improvement fee.
	if err := m.Keeper.Liquidate(ctx, msg.VictimAccountIndex, msg.MarketIndex, msg.BaseAmount); err != nil {
		return nil, err
	}
	return &types.MsgLiquidateResponse{}, nil
}

func (m msgServer) Deleverage(ctx context.Context, msg *types.MsgDeleverage) (*types.MsgDeleverageResponse, error) {
	if err := msg.ValidateBasic(); err != nil {
		return nil, err
	}
	base := msg.BaseAmount
	if base == 0 {
		base = 1
	}
	// IF/Pool deleveragers are governance-only; user ADL requires
	// the deleverager-account owner (master/sub) as sender.
	deleverager, err := m.accountKeeper.GetAccount(ctx, msg.DeleveragerAccountIndex)
	if err != nil {
		return nil, err
	}
	isPool := deleverager.IsPoolType()
	if isPool {
		if msg.Sender != m.authority {
			return nil, types.ErrUnauthorized.Wrapf(
				"pool/if deleverage requires governance authority",
			)
		}
	} else {
		ok, err := m.accountKeeper.IsAuthorized(ctx, msg.Sender, msg.DeleveragerAccountIndex)
		if err != nil {
			return nil, err
		}
		if !ok {
			return nil, types.ErrUnauthorized.Wrapf(
				"sender=%s cannot drive deleverager_account_index=%d",
				msg.Sender, msg.DeleveragerAccountIndex,
			)
		}
	}
	if err := m.Keeper.Deleverage(ctx, msg.VictimAccountIndex, msg.MarketIndex, msg.DeleveragerAccountIndex, base); err != nil {
		return nil, err
	}
	return &types.MsgDeleverageResponse{}, nil
}
