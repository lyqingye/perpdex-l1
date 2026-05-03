package msg

import (
	sdk "github.com/cosmos/cosmos-sdk/types"

	perp "github.com/perpdex/perpdex-l1/app"
	liquidationkeeper "github.com/perpdex/perpdex-l1/x/liquidation/keeper"
	liquidationtypes "github.com/perpdex/perpdex-l1/x/liquidation/types"
	"github.com/perpdex/perpdex-l1/tests/e2e/common"
)

// Liquidate is the keeper-bot path: any signer can attempt to liquidate
// `victim`'s position in `marketIndex` if their health crossed into
// PARTIAL/FULL_LIQUIDATION. The closed position is credited to
// `bot.AccountIndex` — set to 0 to fall back to the sender's master.
func Liquidate(
	app *perp.PerpDEXApp,
	ctx sdk.Context,
	bot common.TestUser,
	victim uint64,
	marketIndex uint32,
	baseAmount uint64,
) (*liquidationtypes.MsgLiquidateResponse, error) {
	srv := liquidationkeeper.NewMsgServerImpl(app.LiquidationKeeper)
	return srv.Liquidate(ctx, &liquidationtypes.MsgLiquidate{
		Sender:                 bot.Address.String(),
		VictimAccountIndex:     victim,
		MarketIndex:            marketIndex,
		BaseAmount:             baseAmount,
		LiquidatorAccountIndex: bot.AccountIndex,
	})
}

// Deleverage is the BANKRUPTCY-state escape hatch: an opposing position
// (`deleverager`) is paired with `victim` at the zero price.
func Deleverage(
	app *perp.PerpDEXApp,
	ctx sdk.Context,
	caller common.TestUser,
	victim, deleverager uint64,
	marketIndex uint32,
	baseAmount uint64,
) (*liquidationtypes.MsgDeleverageResponse, error) {
	srv := liquidationkeeper.NewMsgServerImpl(app.LiquidationKeeper)
	return srv.Deleverage(ctx, &liquidationtypes.MsgDeleverage{
		Sender:                  caller.Address.String(),
		VictimAccountIndex:      victim,
		MarketIndex:             marketIndex,
		DeleveragerAccountIndex: deleverager,
		BaseAmount:              baseAmount,
	})
}
