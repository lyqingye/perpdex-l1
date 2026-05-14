package keeper

import (
	"context"
	"errors"
	"strconv"

	sdk "github.com/cosmos/cosmos-sdk/types"

	"github.com/perpdex/perpdex-l1/x/orderbook/types"
)

// EndBlocker walks the GTT ExpiryIndex (ordered ascending by expiry
// timestamp) and cancels every order whose expiry has elapsed.
//
// The keyset only carries GTT orders that are currently open / partially
// filled / trigger-pending — terminal-status orders have already
// been removed by FillMakerOrder / EvictMakerOrder / CancelOrder
// — so we never run a full Orders-table scan per block.
//
// Trigger handling (which spawns matching) is owned by x/matching.
//
// Per-order failures are isolated: `ErrOrderNotCancelable` is swallowed
// silently (an in-block EvictMakerOrder may already have cancelled the
// maker; the keyset cleanup is idempotent), and any other error emits
// an `EventTypeExpirySweepError` event and continues with the next
// expired order — a single corrupt key cannot stall the whole sweep
// and trip the Cosmos `EndBlocker` panic guard.
func (k Keeper) EndBlocker(ctx context.Context) error {
	sdkCtx := sdk.UnwrapSDKContext(ctx)
	now := sdkCtx.BlockTime().UnixMilli()
	expired, err := k.collectExpiredOrders(ctx, now)
	if err != nil {
		return err
	}
	for _, idx := range expired {
		if _, err := k.CancelOrder(ctx, idx); err != nil {
			if errors.Is(err, types.ErrOrderNotCancelable) {
				continue
			}
			sdkCtx.EventManager().EmitEvent(sdk.NewEvent(
				types.EventTypeExpirySweepError,
				sdk.NewAttribute(types.AttributeKeyOrderIndex, strconv.FormatUint(idx, 10)),
				sdk.NewAttribute(types.AttributeKeyErr, err.Error()),
			))
		}
	}
	return nil
}

// collectExpiredOrders scans the ExpiryIndex in ascending expiry order
// and returns every order_index whose expiry is <= now. Iteration stops
// at the first non-expired key — the keyset is sorted by `(expiry,
// order_index)` so all later entries are guaranteed to be in the
// future.
func (k Keeper) collectExpiredOrders(ctx context.Context, now int64) ([]uint64, error) {
	iter, err := k.ExpiryIndex.Iterate(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer iter.Close()
	var expired []uint64
	for ; iter.Valid(); iter.Next() {
		key, err := iter.Key()
		if err != nil {
			return nil, err
		}
		expiry := key.K1()
		if expiry > now {
			break
		}
		expired = append(expired, key.K2())
	}
	return expired, nil
}
