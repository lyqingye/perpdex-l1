package keeper

import (
	"context"

	sdk "github.com/cosmos/cosmos-sdk/types"

	perptypes "github.com/perpdex/perpdex-l1/types"
)

// EndBlocker scans for expired GTT orders and cancels them via the unified
// CancelOrder lifecycle. Trigger handling (which spawns matching) is owned
// by x/matching.
//
// Orders that have already reached a terminal status (Filled / Cancelled)
// are skipped: the matching loop may have evicted a GTT-expired maker as
// part of an in-block fill via EvictMakerOrder, in which case re-cancelling
// here would error with ErrOrderNotCancelable.
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
		if v.TimeInForce != perptypes.GTT || v.Expiry == 0 || now < v.Expiry {
			continue
		}
		switch v.Status {
		case perptypes.OrderStatusOpen,
			perptypes.OrderStatusPartiallyFilled,
			perptypes.OrderStatusTriggeredPending:
			expired = append(expired, v.OrderIndex)
		}
	}
	iter.Close()

	for _, idx := range expired {
		if _, err := k.CancelOrder(ctx, idx); err != nil {
			return err
		}
	}
	return nil
}
