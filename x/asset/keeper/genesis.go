package keeper

import (
	"context"

	"github.com/perpdex/perpdex-l1/x/asset/types"
)

// InitGenesis seeds the module from a GenesisState.
func (k Keeper) InitGenesis(ctx context.Context, gs types.GenesisState) error {
	if err := k.Params.Set(ctx, gs.Params); err != nil {
		return err
	}
	for _, a := range gs.Assets {
		if err := k.SetAsset(ctx, a); err != nil {
			return err
		}
	}
	if err := k.NextAssetIndex.Set(ctx, uint64(gs.NextAssetIndex)); err != nil {
		return err
	}
	return nil
}

// ExportGenesis serializes the module state.
func (k Keeper) ExportGenesis(ctx context.Context) (*types.GenesisState, error) {
	p, err := k.Params.Get(ctx)
	if err != nil {
		return nil, err
	}
	assets, err := k.AllAssets(ctx)
	if err != nil {
		return nil, err
	}
	next, err := k.NextAssetIndex.Peek(ctx)
	if err != nil {
		return nil, err
	}
	return &types.GenesisState{Params: p, Assets: assets, NextAssetIndex: uint32(next)}, nil
}
