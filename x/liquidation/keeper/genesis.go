package keeper

import (
	"context"

	"github.com/perpdex/perpdex-l1/x/liquidation/types"
)

func (k Keeper) InitGenesis(ctx context.Context, gs types.GenesisState) error {
	if err := k.Params.Set(ctx, gs.Params); err != nil {
		return err
	}
	for _, f := range gs.Flags {
		if err := k.setFlag(ctx, f); err != nil {
			return err
		}
	}
	return nil
}
func (k Keeper) ExportGenesis(ctx context.Context) (*types.GenesisState, error) {
	p, err := k.Params.Get(ctx)
	if err != nil {
		return nil, err
	}
	flags := []types.LiquidationFlag{}
	iter, err := k.Flags.Iterate(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer iter.Close()
	for ; iter.Valid(); iter.Next() {
		v, err := iter.Value()
		if err != nil {
			return nil, err
		}
		flags = append(flags, v)
	}
	return &types.GenesisState{Params: p, Flags: flags}, nil
}
