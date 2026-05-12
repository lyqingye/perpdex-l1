package keeper

import (
	"context"

	perptypes "github.com/perpdex/perpdex-l1/types"
	"github.com/perpdex/perpdex-l1/x/account/types"
)

func (k Keeper) InitGenesis(ctx context.Context, gs types.GenesisState) error {
	if err := k.Params.Set(ctx, gs.Params); err != nil {
		return err
	}
	// createAccount writes the canonical row PLUS every dependent
	// secondary index (OwnerToIndex for masters, MasterSubAccounts
	// for sub / pool rows), so iterating gs.Accounts here is enough
	// to rehydrate every index without an extra pass.
	for _, a := range gs.Accounts {
		if err := k.createAccount(ctx, a); err != nil {
			return err
		}
	}
	for _, aa := range gs.AccountAssets {
		if err := k.setAccountAsset(ctx, aa); err != nil {
			return err
		}
	}
	for _, p := range gs.AccountPositions {
		if err := k.setPosition(ctx, p); err != nil {
			return err
		}
	}
	for _, am := range gs.AccountMetas {
		if err := k.AccountMetas.Set(ctx, am.AccountIndex, am); err != nil {
			return err
		}
	}
	masterCounter := gs.Counters.NextMasterAccountIndex
	if masterCounter < perptypes.FirstUserMasterAccountIndex {
		// Defensive: never seed below the reserved master range so
		// the next deposit-auto-create never reuses a genesis slot.
		masterCounter = perptypes.FirstUserMasterAccountIndex
	}
	if err := k.NextMasterIndex.Set(ctx, masterCounter); err != nil {
		return err
	}
	subCounter := gs.Counters.NextSubAccountIndex
	if subCounter < perptypes.MinSubAccountIndex {
		// Genesis Validate() permits 0 as an unset sentinel; coerce it
		// (and any other below-floor value) to MinSubAccountIndex so
		// the first allocation lands in the valid sub-account range.
		subCounter = perptypes.MinSubAccountIndex
	}
	if err := k.NextSubIndex.Set(ctx, subCounter); err != nil {
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

	// Iterate every `AccountAsset`, `AccountPosition` and `AccountMeta` row
	// so that state export -> import is lossless. Without these iterators
	// spot balances / perp positions / per-account bookkeeping were
	// silently dropped on genesis round-trip.
	assets := []types.AccountAsset{}
	{
		iter, err := k.AccountAssets.Iterate(ctx, nil)
		if err != nil {
			return nil, err
		}
		for ; iter.Valid(); iter.Next() {
			v, err := iter.Value()
			if err != nil {
				iter.Close()
				return nil, err
			}
			assets = append(assets, v)
		}
		iter.Close()
	}
	positions := []types.AccountPosition{}
	{
		iter, err := k.AccountPositions.Iterate(ctx, nil)
		if err != nil {
			return nil, err
		}
		for ; iter.Valid(); iter.Next() {
			v, err := iter.Value()
			if err != nil {
				iter.Close()
				return nil, err
			}
			positions = append(positions, v)
		}
		iter.Close()
	}
	metas := []types.AccountMeta{}
	{
		iter, err := k.AccountMetas.Iterate(ctx, nil)
		if err != nil {
			return nil, err
		}
		for ; iter.Valid(); iter.Next() {
			v, err := iter.Value()
			if err != nil {
				iter.Close()
				return nil, err
			}
			metas = append(metas, v)
		}
		iter.Close()
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
		Params:           p,
		Counters:         types.Counters{NextMasterAccountIndex: master, NextSubAccountIndex: sub},
		Accounts:         accounts,
		AccountAssets:    assets,
		AccountPositions: positions,
		AccountMetas:     metas,
	}, nil
}
