// Package ante hosts the oracle module's contributions to the chain
// ante-handler chain. The single decorator implemented here recognises
// the proposer-injected `MsgAggregateOracleVotes` transaction and
// short-circuits the rest of the ante chain so the empty signature does
// not get rejected by `SigVerificationDecorator`.
package ante

import (
	sdk "github.com/cosmos/cosmos-sdk/types"

	"github.com/perpdex/perpdex-l1/x/oracle/types"
)

// IsOracleInjectedTx returns true when the transaction carries exactly
// one MsgAggregateOracleVotes (the shape produced by the oracle
// VoteExtensionHandler.PrepareProposal). The check is structural only —
// authority validation lives in OracleInjectedTxDecorator.
func IsOracleInjectedTx(tx sdk.Tx) bool {
	msgs := tx.GetMsgs()
	if len(msgs) != 1 {
		return false
	}
	_, ok := msgs[0].(*types.MsgAggregateOracleVotes)
	return ok
}

// OracleInjectedTxDecorator is the only ante decorator on the dedicated
// "oracle injected" ante chain. It exists to ensure that this specific
// transaction shape never reaches a user-facing CheckTx path and that
// the embedded MsgAggregateOracleVotes is signed by the configured gov
// authority.
type OracleInjectedTxDecorator struct {
	govAuthority string
}

// NewOracleInjectedTxDecorator returns a decorator that accepts oracle
// aggregation transactions with `Authority == govAuthority` only when
// they reach the chain through DeliverTx (i.e. were proposer-injected).
func NewOracleInjectedTxDecorator(govAuthority string) OracleInjectedTxDecorator {
	return OracleInjectedTxDecorator{govAuthority: govAuthority}
}

func (d OracleInjectedTxDecorator) AnteHandle(ctx sdk.Context, tx sdk.Tx, simulate bool, next sdk.AnteHandler) (sdk.Context, error) {
	// Reject any attempt to broadcast this Msg through CheckTx. Without
	// this guard a malicious user could submit a signed MsgAggregateOracleVotes
	// against the gov authority address (which would still fail downstream
	// because the keeper rejects it, but at the cost of mempool churn).
	if ctx.IsCheckTx() && !simulate {
		return ctx, types.ErrInjectedTxBlocked.Wrap("MsgAggregateOracleVotes can only be proposer-injected")
	}
	msgs := tx.GetMsgs()
	if len(msgs) != 1 {
		return ctx, types.ErrInjectedTxBlocked.Wrap("oracle injected tx must contain exactly one Msg")
	}
	agg, ok := msgs[0].(*types.MsgAggregateOracleVotes)
	if !ok {
		return ctx, types.ErrInjectedTxBlocked.Wrap("expected MsgAggregateOracleVotes")
	}
	if agg.Authority != d.govAuthority {
		return ctx, types.ErrInvalidAuthority.Wrapf("expected authority=%s got=%s", d.govAuthority, agg.Authority)
	}
	if err := agg.ValidateBasic(); err != nil {
		return ctx, err
	}
	return next(ctx, tx, simulate)
}
