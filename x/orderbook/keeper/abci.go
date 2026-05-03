package keeper

import (
	"context"

	sdk "github.com/cosmos/cosmos-sdk/types"

	perptypes "github.com/perpdex/perpdex-l1/types"
)

// EndBlocker scans for expired GTT orders and removes them. Trigger handling
// (which spawns matching) is owned by x/matching.
func (k Keeper) EndBlocker(ctx context.Context) error {
	now := sdk.UnwrapSDKContext(ctx).BlockTime().UnixMilli()
	iter, err := k.Orders.Iterate(ctx, nil)
	if err != nil {
		return err
	}
	expired := []uint64{}
	for ; iter.Valid(); iter.Next() {
		v, err := iter.Value()
		if err != nil {
			iter.Close()
			return err
		}
		if v.TimeInForce == perptypes.GTT && v.Expiry > 0 && now >= v.Expiry {
			expired = append(expired, v.OrderIndex)
		}
	}
	iter.Close()

	for _, idx := range expired {
		o, err := k.GetOrder(ctx, idx)
		if err != nil {
			continue
		}
		if err := k.RemoveOrderbookEntry(ctx, o.MarketIndex, o.IsAsk, o.OrderIndex); err != nil {
			return err
		}
		_ = k.UnindexClientOrder(ctx, o)
		o.Status = perptypes.OrderStatusCancelled
		if err := k.SetOrder(ctx, o); err != nil {
			return err
		}
	}
	return nil
}
