package keeper

import (
	"context"

	"github.com/perpdex/perpdex-l1/x/matching/types"
)

func (k Keeper) InitGenesis(ctx context.Context, gs types.GenesisState) error {
	return k.Params.Set(ctx, gs.Params)
}
func (k Keeper) ExportGenesis(ctx context.Context) (*types.GenesisState, error) {
	p, err := k.Params.Get(ctx)
	if err != nil {
		return nil, err
	}
	return &types.GenesisState{Params: p}, nil
}
