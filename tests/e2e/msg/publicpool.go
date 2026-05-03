package msg

import (
	"cosmossdk.io/math"

	sdk "github.com/cosmos/cosmos-sdk/types"

	perp "github.com/perpdex/perpdex-l1/app"
	"github.com/perpdex/perpdex-l1/tests/e2e/common"
	accountkeeper "github.com/perpdex/perpdex-l1/x/account/keeper"
	accounttypes "github.com/perpdex/perpdex-l1/x/account/types"
)

// CreatePublicPool spawns a new PUBLIC_POOL sub-account under the
// signer's master and returns its index. The signer's master must
// already exist (e.g. via a prior DepositUSDC) and hold enough
// collateral to seed `initial_total_shares * INITIAL_POOL_SHARE_VALUE`.
func CreatePublicPool(
	app *perp.PerpDEXApp,
	ctx sdk.Context,
	signer common.TestUser,
	masterIdx uint64,
	initialTotalShares uint64,
	operatorFee, minOperatorShareRate uint32,
) (*accounttypes.MsgCreatePublicPoolResponse, error) {
	srv := accountkeeper.NewMsgServerImpl(app.PerpAccountKeeper)
	return srv.CreatePublicPool(ctx, &accounttypes.MsgCreatePublicPool{
		Sender:               signer.Address.String(),
		MasterAccountIndex:   masterIdx,
		AccountType:          2, // PUBLIC_POOL_ACCOUNT_TYPE
		InitialTotalShares:   initialTotalShares,
		OperatorFee:          operatorFee,
		MinOperatorShareRate: minOperatorShareRate,
	})
}

// UpdatePublicPool flips status / fee / min_rate. Caller must be the
// pool operator (master owner for regular pools, gov authority for IF).
func UpdatePublicPool(
	app *perp.PerpDEXApp,
	ctx sdk.Context,
	sender string,
	poolIdx uint64,
	newStatus, newFee, newMinRate uint32,
) (*accounttypes.MsgUpdatePublicPoolResponse, error) {
	srv := accountkeeper.NewMsgServerImpl(app.PerpAccountKeeper)
	return srv.UpdatePublicPool(ctx, &accounttypes.MsgUpdatePublicPool{
		Sender:                   sender,
		PoolAccountIndex:         poolIdx,
		NewStatus:                newStatus,
		NewOperatorFee:           newFee,
		NewMinOperatorShareRate:  newMinRate,
	})
}

// MintShares deposits `principalAmount` uusdc from sender's master into
// the pool and returns the share amount minted.
func MintShares(
	app *perp.PerpDEXApp,
	ctx sdk.Context,
	signer common.TestUser,
	poolIdx, principalAmount uint64,
) (*accounttypes.MsgMintSharesResponse, error) {
	srv := accountkeeper.NewMsgServerImpl(app.PerpAccountKeeper)
	return srv.MintShares(ctx, &accounttypes.MsgMintShares{
		Sender:           signer.Address.String(),
		PoolAccountIndex: poolIdx,
		PrincipalAmount:  principalAmount,
	})
}

// BurnShares redeems `shareAmount` from the pool back into the signer's
// master collateral.
func BurnShares(
	app *perp.PerpDEXApp,
	ctx sdk.Context,
	signer common.TestUser,
	poolIdx uint64,
	shareAmount math.Int,
) (*accounttypes.MsgBurnSharesResponse, error) {
	srv := accountkeeper.NewMsgServerImpl(app.PerpAccountKeeper)
	return srv.BurnShares(ctx, &accounttypes.MsgBurnShares{
		Sender:           signer.Address.String(),
		PoolAccountIndex: poolIdx,
		ShareAmount:      shareAmount,
	})
}

// StrategyTransfer moves `amount` from `from` strategy bucket to `to`
// on the IF pool. Only valid when the pool is INSURANCE_FUND + ACTIVE
// and signer is the operator.
func StrategyTransfer(
	app *perp.PerpDEXApp,
	ctx sdk.Context,
	sender string,
	poolIdx uint64,
	from, to uint32,
	amount math.Int,
) (*accounttypes.MsgStrategyTransferResponse, error) {
	srv := accountkeeper.NewMsgServerImpl(app.PerpAccountKeeper)
	return srv.StrategyTransfer(ctx, &accounttypes.MsgStrategyTransfer{
		Sender:           sender,
		PoolAccountIndex: poolIdx,
		FromStrategy:     from,
		ToStrategy:       to,
		Amount:           amount,
	})
}

// ForceBurnShares is the gov-authority break-glass burn on the LLP /
// IF pool. Bypasses the cooldown.
func ForceBurnShares(
	app *perp.PerpDEXApp,
	ctx sdk.Context,
	authority sdk.AccAddress,
	poolIdx, depositorIdx uint64,
	shareAmount math.Int,
) (*accounttypes.MsgForceBurnSharesResponse, error) {
	srv := accountkeeper.NewMsgServerImpl(app.PerpAccountKeeper)
	return srv.ForceBurnShares(ctx, &accounttypes.MsgForceBurnShares{
		Authority:             authority.String(),
		PoolAccountIndex:      poolIdx,
		DepositorAccountIndex: depositorIdx,
		ShareAmount:           shareAmount,
	})
}
