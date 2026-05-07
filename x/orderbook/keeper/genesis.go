package keeper

import (
	"context"

	perptypes "github.com/perpdex/perpdex-l1/types"
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
		if err := k.restoreGenesisOrderIndexes(ctx, o); err != nil {
			return err
		}
		if err := k.setOrder(ctx, o); err != nil {
			return err
		}
	}
	return nil
}

func (k Keeper) restoreGenesisOrderIndexes(ctx context.Context, o types.Order) error {
	switch o.Status {
	case perptypes.OrderStatusOpen, perptypes.OrderStatusPartiallyFilled:
		entry := types.OrderBookEntry{
			OrderIndex:          o.OrderIndex,
			OwnerAccountIndex:   o.OwnerAccountIndex,
			Price:               o.Price,
			Nonce:               o.Nonce,
			RemainingBaseAmount: o.RemainingBaseAmount,
			Expiry:              o.Expiry,
			IsPostOnly:          o.TimeInForce == perptypes.PostOnly,
			ReduceOnly:          o.ReduceOnly,
			OrderType:           o.OrderType,
		}
		if err := k.insertOrderbookEntry(ctx, o.MarketIndex, o.IsAsk, entry); err != nil {
			return err
		}
		if err := k.indexClientOrder(ctx, o); err != nil {
			return err
		}
		return k.indexAccountOpenOrder(ctx, o)
	case perptypes.OrderStatusTriggeredPending:
		if err := k.addTrigger(ctx, o.MarketIndex, o.TriggerPrice, o.OrderIndex); err != nil {
			return err
		}
		if err := k.indexClientOrder(ctx, o); err != nil {
			return err
		}
		return k.indexAccountOpenOrder(ctx, o)
	default:
		return nil
	}
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
