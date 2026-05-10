package keeper

import (
	"context"
	"strconv"

	"cosmossdk.io/collections"

	sdk "github.com/cosmos/cosmos-sdk/types"

	perptypes "github.com/perpdex/perpdex-l1/types"
	accounttypes "github.com/perpdex/perpdex-l1/x/account/types"
	"github.com/perpdex/perpdex-l1/x/liquidation/types"
	tradekeeper "github.com/perpdex/perpdex-l1/x/trade/keeper"
)

// ApplyExitPosition is invoked by x/market when a market expires. It closes
// every open position in `marketIdx` against the insurance fund at the
// last mark price. Trades carry NoFee + NoRiskCheck so the insurance fund
// can absorb residual size even when doing so worsens its own health.
func (k Keeper) ApplyExitPosition(ctx context.Context, marketIdx uint32) error {
	md, err := k.marketKeeper.GetMarketDetails(ctx, marketIdx)
	if err != nil {
		return err
	}
	closePrice := md.MarkPrice
	if closePrice == 0 {
		// Without a mark price we cannot price the exit. Skip gracefully.
		sdk.UnwrapSDKContext(ctx).Logger().Error(
			"liquidation: skip exit position, mark price unset",
			"market", marketIdx,
		)
		return nil
	}
	sdkCtx := sdk.UnwrapSDKContext(ctx)
	closed := uint32(0)
	if err := k.accountKeeper.IterateAccounts(ctx, func(a accounttypes.Account) bool {
		if a.AccountIndex == perptypes.InsuranceFundOperatorAccountIdx {
			return false
		}
		pos, err := k.accountKeeper.GetPosition(ctx, a.AccountIndex, marketIdx)
		if err != nil || pos.BaseSize.IsZero() {
			return false
		}
		baseAmount := pos.BaseSize.Abs().Uint64()
		if baseAmount == 0 {
			return false
		}
		takerIsAsk := pos.BaseSize.IsNegative()
		if err := k.tradeKeeper.ApplyPerpsMatching(ctx, tradekeeper.PerpFill{
			MakerAccountIndex: a.AccountIndex,
			TakerAccountIndex: perptypes.InsuranceFundOperatorAccountIdx,
			MarketIndex:       marketIdx,
			Price:             closePrice,
			BaseAmount:        baseAmount,
			IsTakerAsk:        takerIsAsk,
			NoFee:             true,
			NoRiskCheck:       true,
		}); err != nil {
			sdkCtx.Logger().Error(
				"liquidation: exit close failed",
				"market", marketIdx,
				"victim", a.AccountIndex,
				"err", err,
			)
			return false
		}
		_ = k.Flags.Remove(ctx, collections.Join(a.AccountIndex, marketIdx))
		closed++
		return false
	}); err != nil {
		return err
	}
	sdkCtx.EventManager().EmitEvent(sdk.NewEvent(
		types.EventTypeMarketExitPosition,
		sdk.NewAttribute(types.AttributeKeyMarketIndex, strconv.FormatUint(uint64(marketIdx), 10)),
		sdk.NewAttribute(types.AttributeKeyClosePrice, strconv.FormatUint(uint64(closePrice), 10)),
		sdk.NewAttribute(types.AttributeKeyClosedPositions, strconv.FormatUint(uint64(closed), 10)),
	))
	return nil
}
