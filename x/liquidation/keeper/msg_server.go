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
	// Resolve the absorbing liquidator account. Callers either set
	// LiquidatorAccountIndex explicitly or let it default to 0, in which
	// case we fall back to the sender's master account.
	liquidator := msg.LiquidatorAccountIndex
	if liquidator == 0 {
		master, err := m.accountKeeper.GetMasterAccountByOwner(ctx, msg.Sender)
		if err != nil {
			return nil, err
		}
		liquidator = master.AccountIndex
	} else {
		// When a specific account is requested, the sender must be
		// authorised to operate it (master / sub of the same owner).
		acc, err := m.accountKeeper.GetAccount(ctx, liquidator)
		if err != nil {
			return nil, err
		}
		if acc.OwnerAddress != msg.Sender {
			return nil, types.ErrUnauthorized.Wrapf(
				"sender=%s cannot use liquidator_account_index=%d", msg.Sender, liquidator,
			)
		}
	}
	if err := m.Keeper.Liquidate(ctx, msg.VictimAccountIndex, msg.MarketIndex, msg.BaseAmount, liquidator); err != nil {
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
	if err := m.Keeper.Deleverage(ctx, msg.VictimAccountIndex, msg.MarketIndex, msg.DeleveragerAccountIndex, base); err != nil {
		return nil, err
	}
	return &types.MsgDeleverageResponse{}, nil
}
