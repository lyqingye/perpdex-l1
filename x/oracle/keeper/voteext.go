package keeper

import (
	"bytes"
	"errors"
	"fmt"

	abci "github.com/cometbft/cometbft/abci/types"
	cmtproto "github.com/cometbft/cometbft/proto/tendermint/types"

	sdk "github.com/cosmos/cosmos-sdk/types"

	abcicodec "github.com/perpdex/perpdex-l1/x/oracle/abci/codec"
	abcive "github.com/perpdex/perpdex-l1/x/oracle/abci/ve"
	"github.com/perpdex/perpdex-l1/x/oracle/types"
)

// NumInjectedTxs is the number of proposer-injected byte streams the
// oracle ABCI handler prepends to a block proposal. Currently fixed at 1
// (the encoded ExtendedCommitInfo) but kept as a named constant so future
// extensions (e.g. an MEV ackowledgement bundle) can be wired in without a
// magic-number search.
const NumInjectedTxs = 1

// OracleInfoIndex is the position of the proposer-injected
// ExtendedCommitInfo bytes inside the block's tx slice.
const OracleInfoIndex = 0

// VoteExtensionHandler bundles the four ABCI++ handlers required by the
// dydx/Slinky-style oracle pipeline.
//
// Lifecycle of one block (height H, vote extensions enabled):
//
//   ┌───────────────────────────────────────────────────────────────────┐
//   │ T0  ExtendVote (every validator, off-consensus goroutine)        │
//   │       reads daemon.Cache → marshals OracleVote → veCodec.Encode   │
//   │ T1  VerifyVoteExtension (every validator on each peer's VE)       │
//   │       veCodec.Decode + per-VE schema check                        │
//   │ T2  CometBFT ships LocalLastCommit (height H-1) to proposer of H  │
//   │ T3  PrepareProposal (proposer of H)                               │
//   │       ecCodec.Encode(req.LocalLastCommit) → Txs[0]                │
//   │ T4  ProcessProposal (every other validator on the proposed block) │
//   │       ecCodec.Decode(Txs[0]) → ValidateExtendedCommit (2/3+)      │
//   │ T5  PreBlock (every validator on the proposed block, in finalize) │
//   │       ecCodec.Decode(Txs[0]) → weighted median per market →       │
//   │       Keeper.SetPrice(...) writes through to IAVL                 │
//   └───────────────────────────────────────────────────────────────────┘
//
// The pipeline mirrors what dydx v4 does with skip-mev/connect (see
// `abci/proposals` and `abci/preblock/oracle` upstream); the byte
// representation we inject as Txs[0] is the cometbft-native
// ExtendedCommitInfo, not a perpdex-specific SDK message, so PreBlock can
// re-derive the aggregate independently of the proposer.
type VoteExtensionHandler struct {
	keeper Keeper

	veCodec abcicodec.VoteExtensionCodec
	ecCodec abcicodec.ExtendedCommitCodec
}

// NewVoteExtensionHandler constructs a handler bundle. The supplied codecs
// MUST be the same ones registered by the chain's PreBlock — encoding and
// decoding are otherwise asymmetric and the chain will refuse blocks.
func NewVoteExtensionHandler(
	k Keeper,
	veCodec abcicodec.VoteExtensionCodec,
	ecCodec abcicodec.ExtendedCommitCodec,
) *VoteExtensionHandler {
	return &VoteExtensionHandler{keeper: k, veCodec: veCodec, ecCodec: ecCodec}
}

// veEnabled returns true when both the on-chain Params toggle and the
// cometbft consensus param agree that vote-extensions are active for the
// current height.
func (h *VoteExtensionHandler) veEnabled(ctx sdk.Context, height int64) bool {
	params, err := h.keeper.Params.Get(ctx)
	if err != nil || !params.VoteExtensionEnabled {
		return false
	}
	cp := ctx.ConsensusParams()
	if cp.Abci == nil || cp.Abci.VoteExtensionsEnableHeight == 0 {
		return false
	}
	return height > cp.Abci.VoteExtensionsEnableHeight
}

// ExtendVote returns the local oracle daemon's price set as the vote
// extension. With vote_extension_enabled=false (or before the enable
// height) it returns an empty payload so consensus is never blocked.
//
// Errors are deliberately swallowed and replaced with empty payloads:
// returning an error from ExtendVote would crash cometbft, so the
// validator instead opts out of this single block (the proposer will
// weight other validators' votes accordingly).
func (h *VoteExtensionHandler) ExtendVote() sdk.ExtendVoteHandler {
	return func(ctx sdk.Context, req *abci.RequestExtendVote) (*abci.ResponseExtendVote, error) {
		if !h.veEnabled(ctx, req.Height) {
			return &abci.ResponseExtendVote{}, nil
		}
		fetcher := h.keeper.PriceFetcher()
		prices, err := fetcher.FetchPrices(ctx, req.Height)
		if err != nil {
			return &abci.ResponseExtendVote{}, nil
		}
		filtered := make([]types.MarketPrice, 0, len(prices))
		for _, p := range prices {
			if p.IndexPrice == 0 || p.MarkPrice == 0 {
				continue
			}
			filtered = append(filtered, p)
		}
		vote := types.OracleVote{
			Prices:            filtered,
			SubmittedAtHeight: req.Height,
		}
		bz, err := h.veCodec.Encode(vote)
		if err != nil {
			return &abci.ResponseExtendVote{}, nil
		}
		return &abci.ResponseExtendVote{VoteExtension: bz}, nil
	}
}

// VerifyVoteExtension performs stateless validation of a peer's payload.
// Empty payloads are accepted (the validator is opting out). Non-empty
// payloads must decode, advertise the same height, and contain only
// non-zero, non-duplicated prices.
func (h *VoteExtensionHandler) VerifyVoteExtension() sdk.VerifyVoteExtensionHandler {
	return func(ctx sdk.Context, req *abci.RequestVerifyVoteExtension) (*abci.ResponseVerifyVoteExtension, error) {
		if !h.veEnabled(ctx, req.Height) {
			if len(req.VoteExtension) > 0 {
				return reject(), nil
			}
			return accept(), nil
		}
		if len(req.VoteExtension) == 0 {
			return accept(), nil
		}
		v, err := h.veCodec.Decode(req.VoteExtension)
		if err != nil {
			return reject(), nil
		}
		if v.SubmittedAtHeight != req.Height {
			return reject(), nil
		}
		seen := map[uint32]struct{}{}
		for _, mp := range v.Prices {
			if mp.IndexPrice == 0 || mp.MarkPrice == 0 {
				return reject(), nil
			}
			if _, dup := seen[mp.MarketIndex]; dup {
				return reject(), nil
			}
			seen[mp.MarketIndex] = struct{}{}
		}
		return accept(), nil
	}
}

// PrepareProposal injects the previous block's ExtendedCommitInfo as
// Txs[0] of the new block proposal. It does NOT aggregate — that work
// happens in ProcessProposal (re-validation) and PreBlock (state write).
// Aggregating here would force every validator to redo the same work in
// PreBlock anyway; pushing the bytes around verbatim avoids the
// duplication and matches the dydx/Connect upstream architecture.
func (h *VoteExtensionHandler) PrepareProposal(defaultHandler sdk.PrepareProposalHandler) sdk.PrepareProposalHandler {
	return func(ctx sdk.Context, req *abci.RequestPrepareProposal) (*abci.ResponsePrepareProposal, error) {
		if !h.veEnabled(ctx, req.Height) {
			return defaultHandler(ctx, req)
		}
		extInfoBz, err := h.ecCodec.Encode(req.LocalLastCommit)
		if err != nil {
			// Fall through with a normal proposal — at worst the
			// chain misses an oracle update for this block.
			return defaultHandler(ctx, req)
		}
		injectedSize := int64(len(extInfoBz))
		if injectedSize >= req.MaxTxBytes {
			return defaultHandler(ctx, req)
		}
		req.MaxTxBytes -= injectedSize
		resp, err := defaultHandler(ctx, req)
		if err != nil {
			return nil, err
		}
		resp.Txs = injectAndResize(resp.Txs, extInfoBz, req.MaxTxBytes+injectedSize)
		return resp, nil
	}
}

// ProcessProposal verifies that Txs[0] is a well-formed
// ExtendedCommitInfo carrying a 2/3+ supermajority of voting power. On
// success it strips Txs[0] before forwarding to the wrapped default
// handler so the rest of the SDK pipeline does not try to interpret the
// raw bytes as an SDK transaction.
func (h *VoteExtensionHandler) ProcessProposal(defaultHandler sdk.ProcessProposalHandler) sdk.ProcessProposalHandler {
	return func(ctx sdk.Context, req *abci.RequestProcessProposal) (*abci.ResponseProcessProposal, error) {
		if !h.veEnabled(ctx, req.Height) {
			return defaultHandler(ctx, req)
		}
		if len(req.Txs) < NumInjectedTxs {
			return rejectProposal(), nil
		}
		extInfoBz := req.Txs[OracleInfoIndex]
		extInfo, err := h.ecCodec.Decode(extInfoBz)
		if err != nil {
			return rejectProposal(), nil
		}
		if extInfo.Round < 0 {
			return rejectProposal(), nil
		}
		if err := abcive.ValidateExtendedCommit(extInfo); err != nil {
			return rejectProposal(), nil
		}
		// Strip Txs[0] before forwarding so baseapp's TxDecoder doesn't
		// try to interpret the raw cometabci bytes as an SDK Tx.
		req.Txs = req.Txs[NumInjectedTxs:]
		resp, err := defaultHandler(ctx, req)
		if err != nil {
			return rejectProposal(), err
		}
		// Restore Txs[0] so any other caller of `req` sees the same
		// shape we received from cometbft. baseapp doesn't actually
		// re-read req after ProcessProposal returns but downstream
		// hooks (and tests) expect ext info to be present.
		req.Txs = append([][]byte{extInfoBz}, req.Txs...)
		return resp, nil
	}
}

// injectAndResize prepends `injectTx` to `appTxs` while keeping the total
// size <= maxSizeBytes. Mirrors the upstream Connect helper.
func injectAndResize(appTxs [][]byte, injectTx []byte, maxSizeBytes int64) [][]byte {
	var (
		returned        [][]byte
		consumedBytes   int64
		alreadyInjected bool
	)
	if len(appTxs) > 0 && bytes.Equal(appTxs[0], injectTx) {
		alreadyInjected = true
	}
	if !alreadyInjected {
		injectBytes := int64(len(injectTx))
		if injectBytes <= maxSizeBytes {
			consumedBytes += injectBytes
			returned = append(returned, injectTx)
		}
	}
	for _, tx := range appTxs {
		if alreadyInjected {
			alreadyInjected = false
			returned = append(returned, tx)
			consumedBytes += int64(len(tx))
			continue
		}
		consumedBytes += int64(len(tx))
		if consumedBytes > maxSizeBytes {
			return returned
		}
		returned = append(returned, tx)
	}
	return returned
}

func accept() *abci.ResponseVerifyVoteExtension {
	return &abci.ResponseVerifyVoteExtension{Status: abci.ResponseVerifyVoteExtension_ACCEPT}
}
func reject() *abci.ResponseVerifyVoteExtension {
	return &abci.ResponseVerifyVoteExtension{Status: abci.ResponseVerifyVoteExtension_REJECT}
}
func rejectProposal() *abci.ResponseProcessProposal {
	return &abci.ResponseProcessProposal{Status: abci.ResponseProcessProposal_REJECT}
}

// CountCommittedVotes returns the (committed power, total power) pair from
// an ExtendedCommitInfo. Exposed for telemetry hooks.
func CountCommittedVotes(ext abci.ExtendedCommitInfo) (committedPower, totalPower int64) {
	for _, v := range ext.Votes {
		totalPower += v.Validator.Power
		if v.BlockIdFlag == cmtproto.BlockIDFlagCommit {
			committedPower += v.Validator.Power
		}
	}
	return
}

// ErrInvalidExtendedCommit is the error type returned by validate-only
// helpers that need a sentinel for ProcessProposal callers.
var ErrInvalidExtendedCommit = errors.New("oracle: invalid extended commit")

// describeRejection wraps abci.ResponseProcessProposal with a string so
// log lines can give operators a cause without having to plumb errors
// through cometbft's response shape.
func describeRejection(reason string) *abci.ResponseProcessProposal {
	_ = fmt.Sprintf("oracle: rejecting proposal: %s", reason)
	return rejectProposal()
}

var _ = describeRejection // retained for future telemetry hooks
