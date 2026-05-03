package keeper

import (
	"context"

	"github.com/perpdex/perpdex-l1/x/funding/types"
)

func (k Keeper) InitGenesis(ctx context.Context, gs types.GenesisState) error {
	if err := k.Params.Set(ctx, gs.Params); err != nil {
		return err
	}
	return k.Metadata.Set(ctx, gs.Metadata)
}

func (k Keeper) ExportGenesis(ctx context.Context) (*types.GenesisState, error) {
	p, err := k.Params.Get(ctx)
	if err != nil {
		return nil, err
	}
	m, err := k.Metadata.Get(ctx)
	if err != nil {
		return nil, err
	}
	return &types.GenesisState{Params: p, Metadata: m}, nil
}
