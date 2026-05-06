package keeper

import (
	"context"
	"strconv"

	sdk "github.com/cosmos/cosmos-sdk/types"

	perptypes "github.com/perpdex/perpdex-l1/types"
	"github.com/perpdex/perpdex-l1/x/market/types"
)

// EndBlocker scans the expiry index and marks expired markets as EXPIRED. It
// also asks the liquidation keeper (when wired) to close any open positions.
func (k Keeper) EndBlocker(ctx context.Context) error {
	now := sdk.UnwrapSDKContext(ctx).BlockTime().UnixMilli()
	iter, err := k.Markets.Iterate(ctx, nil)
	if err != nil {
		return err
	}
	defer iter.Close()
	for ; iter.Valid(); iter.Next() {
		m, err := iter.Value()
		if err != nil {
			return err
		}
		if m.Status == perptypes.MarketStatusExpired {
			continue
		}
		if m.ExpiryTimestamp > 0 && now >= m.ExpiryTimestamp {
			m.Status = perptypes.MarketStatusExpired
			if err := k.SetMarket(ctx, m); err != nil {
				return err
			}
			if err := k.liquidationKeeper.ApplyExitPosition(ctx, m.MarketIndex); err != nil {
				sdk.UnwrapSDKContext(ctx).Logger().Error("market: apply exit failed", "market", m.MarketIndex, "err", err)
			}
			sdk.UnwrapSDKContext(ctx).EventManager().EmitEvent(sdk.NewEvent(
				types.EventTypeMarketExpired,
				sdk.NewAttribute(types.AttributeKeyMarketIndex, strconv.FormatUint(uint64(m.MarketIndex), 10)),
			))
		}
	}
	return nil
}
