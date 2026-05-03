package keeper

import (
	"context"

	"github.com/perpdex/perpdex-l1/x/orderbook/types"
)

func (k Keeper) InitGenesis(ctx context.Context, gs types.GenesisState) error {
	if err := k.Params.Set(ctx, gs.Params); err != nil {
		return err
	}
	if err := k.NextOrderIndex.Set(ctx, gs.NextOrderIndex); err != nil {
		return err
	}
	for _, o := range gs.Orders {
		if err := k.SetOrder(ctx, o); err != nil {
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
	next, err := k.NextOrderIndex.Peek(ctx)
	if err != nil {
		return nil, err
	}
	orders := []types.Order{}
	iter, err := k.Orders.Iterate(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer iter.Close()
	for ; iter.Valid(); iter.Next() {
		v, err := iter.Value()
		if err != nil {
			return nil, err
		}
		orders = append(orders, v)
	}
	return &types.GenesisState{Params: p, NextOrderIndex: next, Orders: orders}, nil
}
