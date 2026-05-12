package keeper

import (
	"context"

	sdk "github.com/cosmos/cosmos-sdk/types"

	"github.com/perpdex/perpdex-l1/x/market/types"
)

// EndBlocker drives auto-expiry. Markets that did not fit in this
// block's budget stay in the ExpiryIndex; the next block picks them
// up because their (expiry, idx) entries are still <= the next now.
func (k Keeper) EndBlocker(ctx context.Context) error {
	params, err := k.Params.Get(ctx)
	if err != nil {
		return err
	}
	now := sdk.UnwrapSDKContext(ctx).BlockTime().UnixMilli()
	return k.iterateExpired(ctx, now, params.MaxMarketsExpiredPerBlock,
		func(m types.Market) error { return k.expireMarket(ctx, m) })
}
