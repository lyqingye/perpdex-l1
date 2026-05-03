package msg

import (
	"cosmossdk.io/math"

	sdk "github.com/cosmos/cosmos-sdk/types"

	perp "github.com/perpdex/perpdex-l1/app"
	perptypes "github.com/perpdex/perpdex-l1/types"
	"github.com/perpdex/perpdex-l1/tests/e2e/common"
	accountkeeper "github.com/perpdex/perpdex-l1/x/account/keeper"
	accounttypes "github.com/perpdex/perpdex-l1/x/account/types"
)

// Deposit credits collateral or spot balance for the user. The first call
// also creates the user's perpdex master account; the resulting
// AccountIndex is recorded back into the supplied user (mutating the
// caller's pointer is intentional and matches the wider e2e style).
func Deposit(
	app *perp.PerpDEXApp,
	ctx sdk.Context,
	user *common.TestUser,
	assetIndex uint32,
	routeType uint32,
	amount uint64,
) (*accounttypes.MsgDepositResponse, error) {
	srv := accountkeeper.NewMsgServerImpl(app.PerpAccountKeeper)
	resp, err := srv.Deposit(ctx, &accounttypes.MsgDeposit{
		Sender:     user.Address.String(),
		AssetIndex: assetIndex,
		RouteType:  routeType,
		Amount:     amount,
	})
	if err != nil {
		return nil, err
	}
	user.AccountIndex = resp.AccountIndex
	return resp, nil
}

// DepositUSDC is the most common variant: deposit USDC into the perp
// collateral route at the canonical USDC asset_index.
func DepositUSDC(
	app *perp.PerpDEXApp,
	ctx sdk.Context,
	user *common.TestUser,
	amount uint64,
) (*accounttypes.MsgDepositResponse, error) {
	return Deposit(app, ctx, user, perptypes.USDCAssetIndex, perptypes.RouteTypePerps, amount)
}

// Withdraw sends `amount` (external precision) of the asset back from the
// perpdex account to the user's bank address.
func Withdraw(
	app *perp.PerpDEXApp,
	ctx sdk.Context,
	user common.TestUser,
	assetIndex uint32,
	routeType uint32,
	amount uint64,
) (*accounttypes.MsgWithdrawResponse, error) {
	srv := accountkeeper.NewMsgServerImpl(app.PerpAccountKeeper)
	return srv.Withdraw(ctx, &accounttypes.MsgWithdraw{
		Sender:       user.Address.String(),
		AccountIndex: user.AccountIndex,
		AssetIndex:   assetIndex,
		RouteType:    routeType,
		Amount:       amount,
	})
}

// Transfer moves collateral or spot balance between two perpdex accounts
// owned by the same signer.
func Transfer(
	app *perp.PerpDEXApp,
	ctx sdk.Context,
	signer common.TestUser,
	fromAccount, toAccount uint64,
	assetIndex uint32,
	amount uint64,
) (*accounttypes.MsgTransferResponse, error) {
	srv := accountkeeper.NewMsgServerImpl(app.PerpAccountKeeper)
	return srv.Transfer(ctx, &accounttypes.MsgTransfer{
		Sender:           signer.Address.String(),
		FromAccountIndex: fromAccount,
		ToAccountIndex:   toAccount,
		AssetIndex:       assetIndex,
		Amount:           amount,
	})
}

// UpdateLeverage flips the margin mode (CrossMargin/IsolatedMargin) and
// initial-margin-fraction on a (closed) position before re-opening.
func UpdateLeverage(
	app *perp.PerpDEXApp,
	ctx sdk.Context,
	user common.TestUser,
	marketIndex uint32,
	newIMF uint32,
	newMarginMode uint32,
) (*accounttypes.MsgUpdateLeverageResponse, error) {
	srv := accountkeeper.NewMsgServerImpl(app.PerpAccountKeeper)
	return srv.UpdateLeverage(ctx, &accounttypes.MsgUpdateLeverage{
		Sender:                   user.Address.String(),
		AccountIndex:             user.AccountIndex,
		MarketIndex:              marketIndex,
		NewInitialMarginFraction: newIMF,
		NewMarginMode:            newMarginMode,
	})
}

// UpdateMargin adds (Action=AddMargin) or removes (Action=RemoveMargin)
// amount of collateral assigned to an isolated position.
func UpdateMargin(
	app *perp.PerpDEXApp,
	ctx sdk.Context,
	user common.TestUser,
	marketIndex uint32,
	action uint32,
	amount math.Int,
) (*accounttypes.MsgUpdateMarginResponse, error) {
	srv := accountkeeper.NewMsgServerImpl(app.PerpAccountKeeper)
	return srv.UpdateMargin(ctx, &accounttypes.MsgUpdateMargin{
		Sender:       user.Address.String(),
		AccountIndex: user.AccountIndex,
		MarketIndex:  marketIndex,
		Action:       action,
		Amount:       amount,
	})
}
