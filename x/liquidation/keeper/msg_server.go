package keeper

import (
	"context"

	perptypes "github.com/perpdex/perpdex-l1/types"
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
	// Sender must be authorised to operate the deleverager account.
	// Insurance Fund / Public Pool deleveragers are protocol-level paths
	// that only the governance authority may drive directly; ADL from
	// a user account is permitted for the account's owner (master/sub
	// of the same bech32 address).
	deleverager, err := m.accountKeeper.GetAccount(ctx, msg.DeleveragerAccountIndex)
	if err != nil {
		return nil, err
	}
	isPool := deleverager.AccountType == perptypes.PublicPoolAccountType ||
		deleverager.AccountType == perptypes.InsuranceFundAccountType
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
