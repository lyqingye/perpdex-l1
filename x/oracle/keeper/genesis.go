package keeper

import (
	"context"

	"github.com/perpdex/perpdex-l1/x/oracle/types"
)

func (k Keeper) InitGenesis(ctx context.Context, gs types.GenesisState) error {
	if err := k.Params.Set(ctx, gs.Params); err != nil {
		return err
	}
	for _, p := range gs.Prices {
		// Genesis may seed placeholder rows with zero prices that will
		// be populated later by the oracle module; bypass the non-zero
		// validation for that single code path.
		if err := k.SetPriceUnsafe(ctx, p); err != nil {
			return err
		}
	}
	for _, p := range gs.Providers {
		if err := k.Providers.Set(ctx, p.Address, p); err != nil {
			return err
		}
	}
	for _, b := range gs.Bindings {
		if err := k.Bindings.Set(ctx, b.ValidatorAddress, b); err != nil {
			return err
		}
		if err := k.OperatorIdx.Set(ctx, b.OracleOperatorAddress, b.ValidatorAddress); err != nil {
			return err
		}
	}
	for _, s := range gs.Stats {
		if err := k.Stats.Set(ctx, s.ValidatorAddress, s); err != nil {
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
	providers, err := k.AllProviders(ctx)
	if err != nil {
		return nil, err
	}
	bindings, err := k.AllBindings(ctx)
	if err != nil {
		return nil, err
	}
	return &types.GenesisState{Params: p, Providers: providers, Bindings: bindings}, nil
}
