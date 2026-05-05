// Package msg provides thin Msg-server wrappers used by the e2e suite.
//
// The oracle helpers exercise the *post-aggregation* msg path
// (MsgAggregateOracleVotes / MsgUpdateParams). The full ABCI++ vote
// extension pipeline is exercised separately by the dedicated oracle
// e2e (which constructs an ExtendedCommitInfo and calls
// VoteExtensionHandler.PrepareProposal directly).
package msg

import (
	sdk "github.com/cosmos/cosmos-sdk/types"

	perp "github.com/perpdex/perpdex-l1/app"
	oraclekeeper "github.com/perpdex/perpdex-l1/x/oracle/keeper"
	oracletypes "github.com/perpdex/perpdex-l1/x/oracle/types"
)

// AggregateVotes invokes the oracle msg server's `AggregateOracleVotes`
// handler directly, simulating what the chain does on every block once
// the proposer's PrepareProposal has built and injected the aggregated
// price tx. Useful for smoke-testing the keeper plumbing in isolation
// from the consensus layer.
func AggregateVotes(
	app *perp.PerpDEXApp,
	ctx sdk.Context,
	govAddr sdk.AccAddress,
	height int64,
	aggregations []oracletypes.MarketAggregation,
) (*oracletypes.MsgAggregateOracleVotesResponse, error) {
	srv := oraclekeeper.NewMsgServerImpl(app.OracleKeeper)
	return srv.AggregateOracleVotes(ctx, &oracletypes.MsgAggregateOracleVotes{
		Authority:    govAddr.String(),
		Height:       height,
		Aggregations: aggregations,
	})
}

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
