// Package msg provides thin Msg-server wrappers used by the e2e suite.
//
// The oracle module exposes only `MsgUpdateParams` as an SDK message;
// price updates land on chain via the ABCI++ vote-extension pipeline
// (PreBlock decodes the proposer-injected ExtendedCommitInfo bytes).
// The dedicated oracle e2e exercises that pipeline directly. Other
// suites that just need to seed prices should use the
// `SetOraclePrice` helper on the e2e suite, which writes through the
// keeper and bypasses the VE pipeline.
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
