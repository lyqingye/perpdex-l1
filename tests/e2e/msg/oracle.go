// Package msg provides thin Msg-server wrappers used by the e2e suite.
//
// After the move to the dydx/Connect-style ABCI++ pipeline, the oracle
// module exposes only `MsgUpdateParams` as an SDK message. Price updates
// land on chain via PreBlock decoding the proposer-injected
// ExtendedCommitInfo bytes; the dedicated oracle e2e exercises that
// pipeline directly. For other suites that just need to seed prices,
// use the `SetOraclePrice` helper on the e2e suite (writes through the
// keeper, bypassing the VE pipeline).
package msg

import (
	sdk "github.com/cosmos/cosmos-sdk/types"

	perp "github.com/perpdex/perpdex-l1/app"
	oraclekeeper "github.com/perpdex/perpdex-l1/x/oracle/keeper"
	oracletypes "github.com/perpdex/perpdex-l1/x/oracle/types"
)

// UpdateOracleParams gov-rotates the oracle module Params.
func UpdateOracleParams(
	app *perp.PerpDEXApp,
	ctx sdk.Context,
	govAddr sdk.AccAddress,
	params oracletypes.Params,
) (*oracletypes.MsgUpdateParamsResponse, error) {
	srv := oraclekeeper.NewMsgServerImpl(app.OracleKeeper)
	return srv.UpdateParams(ctx, &oracletypes.MsgUpdateParams{
		Authority: govAddr.String(),
		Params:    params,
	})
}
