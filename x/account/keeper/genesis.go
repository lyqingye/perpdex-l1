package keeper

import (
	"context"

	"github.com/perpdex/perpdex-l1/x/account/types"
)

func (k Keeper) InitGenesis(ctx context.Context, gs types.GenesisState) error {
	if err := k.Params.Set(ctx, gs.Params); err != nil {
		return err
	}
	for _, a := range gs.Accounts {
		if err := k.SetAccount(ctx, a); err != nil {
			return err
		}
	}
	for _, aa := range gs.AccountAssets {
		if err := k.SetAccountAsset(ctx, aa); err != nil {
			return err
		}
	}
	for _, p := range gs.AccountPositions {
		if err := k.SetPosition(ctx, p); err != nil {
			return err
		}
	}
	for _, am := range gs.AccountMetas {
		if err := k.AccountMetas.Set(ctx, am.AccountIndex, am); err != nil {
			return err
		}
	}
	if err := k.NextMasterIndex.Set(ctx, gs.Counters.NextMasterAccountIndex); err != nil {
		return err
	}
	if err := k.NextSubIndex.Set(ctx, gs.Counters.NextSubAccountIndex); err != nil {
		return err
	}
	return nil
}

func (k Keeper) ExportGenesis(ctx context.Context) (*types.GenesisState, error) {
	p, err := k.Params.Get(ctx)
	if err != nil {
		return nil, err
	}
	accounts := []types.Account{}
	if err := k.IterateAccounts(ctx, func(a types.Account) bool { accounts = append(accounts, a); return false }); err != nil {
		return nil, err
	}
	master, err := k.NextMasterIndex.Peek(ctx)
	if err != nil {
		return nil, err
	}
	sub, err := k.NextSubIndex.Peek(ctx)
	if err != nil {
		return nil, err
	}
	return &types.GenesisState{
		Params:   p,
		Counters: types.Counters{NextMasterAccountIndex: master, NextSubAccountIndex: sub},
		Accounts: accounts,
	}, nil
}
