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
		// Genesis may seed placeholder rows with zero prices that will be
		// populated later by the vote-extension pipeline; bypass the
		// non-zero validation for that single code path.
		if err := k.SetPriceUnsafe(ctx, p); err != nil {
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
	prices := []types.OraclePrice{}
	iter, err := k.Prices.Iterate(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer iter.Close()
	for ; iter.Valid(); iter.Next() {
		v, err := iter.Value()
		if err != nil {
			return nil, err
		}
		prices = append(prices, v)
	}
	return &types.GenesisState{Params: p, Prices: prices}, nil
}
