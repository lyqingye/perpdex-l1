package ante

import (
	ibcante "github.com/cosmos/ibc-go/v10/modules/core/ante"
	ibckeeper "github.com/cosmos/ibc-go/v10/modules/core/keeper"

	errorsmod "cosmossdk.io/errors"

	sdk "github.com/cosmos/cosmos-sdk/types"
	sdkerrors "github.com/cosmos/cosmos-sdk/types/errors"
	"github.com/cosmos/cosmos-sdk/x/auth/ante"

	oracleante "github.com/perpdex/perpdex-l1/x/oracle/ante"
)

// HandlerOptions extends the SDK's ante handler options with the IBC keeper
// (for the redundant-relay decorator) and the oracle gov authority address
// (for the proposer-injected MsgAggregateOracleVotes short-circuit).
type HandlerOptions struct {
	ante.HandlerOptions

	IBCKeeper           *ibckeeper.Keeper
	OracleGovAuthority  string
}

// NewAnteHandler returns the chain's full AnteHandler chain.
//
// Two parallel sub-chains are built:
//
//   - `oracleHandler` runs only the OracleInjectedTxDecorator and is used
//     for transactions whose sole Msg is MsgAggregateOracleVotes (the
//     shape produced by oracle.VoteExtensionHandler.PrepareProposal).
//     This bypasses signature verification because the gov authority
//     module account has no private key and the tx is trusted to have
//     been proposer-injected by virtue of reaching DeliverTx with no
//     signers.
//   - `defaultHandler` is the SDK default chain plus the IBC redundant-
//     relay short-circuit; it handles every other transaction.
//
// The dispatch happens in the returned closure based on tx shape.
func NewAnteHandler(opts HandlerOptions) (sdk.AnteHandler, error) {
	if opts.AccountKeeper == nil {
		return nil, errorsmod.Wrap(sdkerrors.ErrLogic, "account keeper is required for AnteHandler")
	}
	if opts.BankKeeper == nil {
		return nil, errorsmod.Wrap(sdkerrors.ErrLogic, "bank keeper is required for AnteHandler")
	}
	if opts.SignModeHandler == nil {
		return nil, errorsmod.Wrap(sdkerrors.ErrLogic, "sign mode handler is required for AnteHandler")
	}
	if opts.IBCKeeper == nil {
		return nil, errorsmod.Wrap(sdkerrors.ErrLogic, "IBC keeper is required for AnteHandler")
	}
	if opts.OracleGovAuthority == "" {
		return nil, errorsmod.Wrap(sdkerrors.ErrLogic, "oracle gov authority is required for AnteHandler")
	}

	sigGasConsumer := opts.SigGasConsumer
	if sigGasConsumer == nil {
		sigGasConsumer = ante.DefaultSigVerificationGasConsumer
	}

	defaultDecorators := []sdk.AnteDecorator{
		ante.NewSetUpContextDecorator(),
		ante.NewExtensionOptionsDecorator(opts.ExtensionOptionChecker),
		ante.NewValidateBasicDecorator(),
		ante.NewTxTimeoutHeightDecorator(),
		ante.NewValidateMemoDecorator(opts.AccountKeeper),
		ante.NewConsumeGasForTxSizeDecorator(opts.AccountKeeper),
		ante.NewDeductFeeDecorator(opts.AccountKeeper, opts.BankKeeper, opts.FeegrantKeeper, opts.TxFeeChecker),
		ante.NewSetPubKeyDecorator(opts.AccountKeeper),
		ante.NewValidateSigCountDecorator(opts.AccountKeeper),
		ante.NewSigGasConsumeDecorator(opts.AccountKeeper, sigGasConsumer),
		ante.NewSigVerificationDecorator(opts.AccountKeeper, opts.SignModeHandler),
		ante.NewIncrementSequenceDecorator(opts.AccountKeeper),
		ibcante.NewRedundantRelayDecorator(opts.IBCKeeper),
	}
	defaultHandler := sdk.ChainAnteDecorators(defaultDecorators...)

	oracleDecorators := []sdk.AnteDecorator{
		ante.NewSetUpContextDecorator(),
		oracleante.NewOracleInjectedTxDecorator(opts.OracleGovAuthority),
	}
	oracleHandler := sdk.ChainAnteDecorators(oracleDecorators...)

	return func(ctx sdk.Context, tx sdk.Tx, simulate bool) (sdk.Context, error) {
		if oracleante.IsOracleInjectedTx(tx) {
			return oracleHandler(ctx, tx, simulate)
		}
		return defaultHandler(ctx, tx, simulate)
	}, nil
}
