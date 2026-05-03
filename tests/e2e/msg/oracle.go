package msg

import (
	sdk "github.com/cosmos/cosmos-sdk/types"

	perp "github.com/perpdex/perpdex-l1/app"
	oraclekeeper "github.com/perpdex/perpdex-l1/x/oracle/keeper"
	oracletypes "github.com/perpdex/perpdex-l1/x/oracle/types"
)

// AddOracleProvider whitelists `provider` so they can call MsgInjectOracle.
func AddOracleProvider(
	app *perp.PerpDEXApp,
	ctx sdk.Context,
	govAddr sdk.AccAddress,
	provider sdk.AccAddress,
	description string,
) (*oracletypes.MsgAddOracleProviderResponse, error) {
	srv := oraclekeeper.NewMsgServerImpl(app.OracleKeeper)
	return srv.AddOracleProvider(ctx, &oracletypes.MsgAddOracleProvider{
		Authority:   govAddr.String(),
		Address:     provider.String(),
		Description: description,
	})
}

// SetAggregationMode flips the oracle from WHITELIST to PoS_MEDIAN (or
// back); the test driver uses this to simulate a governance switch.
func SetAggregationMode(
	app *perp.PerpDEXApp,
	ctx sdk.Context,
	govAddr sdk.AccAddress,
	newMode uint32,
) (*oracletypes.MsgSetAggregationModeResponse, error) {
	srv := oraclekeeper.NewMsgServerImpl(app.OracleKeeper)
	return srv.SetAggregationMode(ctx, &oracletypes.MsgSetAggregationMode{
		Authority: govAddr.String(),
		NewMode:   newMode,
	})
}

// InjectPrice is the WHITELIST-mode entry: a provider posts a (market ->
// {index, mark}) price map. The helper forwards each entry verbatim so
// callers can drive multi-market scenarios with a single Msg.
func InjectPrice(
	app *perp.PerpDEXApp,
	ctx sdk.Context,
	provider sdk.AccAddress,
	prices []oracletypes.MarketPrice,
) (*oracletypes.MsgInjectOracleResponse, error) {
	srv := oraclekeeper.NewMsgServerImpl(app.OracleKeeper)
	return srv.InjectOracle(ctx, &oracletypes.MsgInjectOracle{
		Sender: provider.String(),
		Prices: prices,
	})
}

// AggregateVotes drives the PoS_MEDIAN code path by sending an
// already-aggregated MsgAggregateOracleVotes (signed by the proposer /
// governance authority). Used by the oracle scenario to simulate the
// vote-extension result without spinning up actual ABCI++ pipelines.
func AggregateVotes(
	app *perp.PerpDEXApp,
	ctx sdk.Context,
	govAddr sdk.AccAddress,
	height int64,
	aggregations []oracletypes.MarketAggregation,
	voters []oracletypes.VoterRecord,
) (*oracletypes.MsgAggregateOracleVotesResponse, error) {
	srv := oraclekeeper.NewMsgServerImpl(app.OracleKeeper)
	return srv.AggregateOracleVotes(ctx, &oracletypes.MsgAggregateOracleVotes{
		Authority:    govAddr.String(),
		Height:       height,
		Aggregations: aggregations,
		VoterRecords: voters,
	})
}

// BindOracleOperator records the (validator, oracle-operator) pair on
// chain. The signer must equal the validator address.
func BindOracleOperator(
	app *perp.PerpDEXApp,
	ctx sdk.Context,
	validatorAddress, operatorAddress, metadata string,
) (*oracletypes.MsgBindOracleOperatorResponse, error) {
	srv := oraclekeeper.NewMsgServerImpl(app.OracleKeeper)
	return srv.BindOracleOperator(ctx, &oracletypes.MsgBindOracleOperator{
		Sender:                validatorAddress,
		ValidatorAddress:      validatorAddress,
		OracleOperatorAddress: operatorAddress,
		Metadata:              metadata,
	})
}
