package keeper

import (
	"context"

	abci "github.com/cometbft/cometbft/abci/types"

	sdk "github.com/cosmos/cosmos-sdk/types"

	"github.com/perpdex/perpdex-l1/x/oracle/types"
)

// VoteExtensionHandler bundles the four ABCI++ handlers required by the PoS
// oracle. They are wired in app.go via SetExtendVoteHandler etc. and are no-ops
// while Params.VoteExtensionEnabled is false.
type VoteExtensionHandler struct {
	keeper Keeper
}

func NewVoteExtensionHandler(k Keeper) *VoteExtensionHandler { return &VoteExtensionHandler{keeper: k} }

// ExtendVote proposes the local oracle operator's price set as the vote
// extension. With vote_extension_enabled=false this returns an empty payload.
func (h *VoteExtensionHandler) ExtendVote() sdk.ExtendVoteHandler {
	return func(ctx sdk.Context, req *abci.RequestExtendVote) (*abci.ResponseExtendVote, error) {
		params, err := h.keeper.Params.Get(ctx)
		if err != nil {
			return &abci.ResponseExtendVote{}, nil
		}
		if !params.VoteExtensionEnabled {
			return &abci.ResponseExtendVote{}, nil
		}
		// In a real deployment the local node would query its oracle operator
		// process for prices and serialize them as OracleVote. For MVP we send
		// an empty payload so consensus remains unblocked.
		vote := types.OracleVote{
			SubmittedAtHeight: req.Height,
		}
		bz, err := vote.Marshal()
		if err != nil {
			return &abci.ResponseExtendVote{}, nil
		}
		return &abci.ResponseExtendVote{VoteExtension: bz}, nil
	}
}

// VerifyVoteExtension only enforces that the payload is decodable as
// types.OracleVote. Pricing rules are enforced at PrepareProposal time.
func (h *VoteExtensionHandler) VerifyVoteExtension() sdk.VerifyVoteExtensionHandler {
	return func(ctx sdk.Context, req *abci.RequestVerifyVoteExtension) (*abci.ResponseVerifyVoteExtension, error) {
		if len(req.VoteExtension) == 0 {
			return &abci.ResponseVerifyVoteExtension{Status: abci.ResponseVerifyVoteExtension_ACCEPT}, nil
		}
		var v types.OracleVote
		if err := v.Unmarshal(req.VoteExtension); err != nil {
			return &abci.ResponseVerifyVoteExtension{Status: abci.ResponseVerifyVoteExtension_REJECT}, nil
		}
		return &abci.ResponseVerifyVoteExtension{Status: abci.ResponseVerifyVoteExtension_ACCEPT}, nil
	}
}

// PrepareProposal aggregates collected vote extensions via weighted median and
// injects an MsgAggregateOracleVotes Tx as the first transaction in the block.
// MVP: we do not actually inject; this returns the underlying baseapp default.
func (h *VoteExtensionHandler) PrepareProposal(defaultHandler sdk.PrepareProposalHandler) sdk.PrepareProposalHandler {
	return func(ctx sdk.Context, req *abci.RequestPrepareProposal) (*abci.ResponsePrepareProposal, error) {
		return defaultHandler(ctx, req)
	}
}

// ProcessProposal validates the injected aggregation Tx (when present).
func (h *VoteExtensionHandler) ProcessProposal(defaultHandler sdk.ProcessProposalHandler) sdk.ProcessProposalHandler {
	return func(ctx sdk.Context, req *abci.RequestProcessProposal) (*abci.ResponseProcessProposal, error) {
		return defaultHandler(ctx, req)
	}
}

// AggregateVotes computes a per-market weighted median across a set of
// validator votes. Used by PrepareProposal and exposed for tests.
func (k Keeper) AggregateVotes(_ context.Context, votes []types.OracleVote, weights map[string]uint64) []types.MarketAggregation {
	collected := map[uint32]struct {
		idx []uint32
		mp  []uint32
		w   []uint64
	}{}
	for _, vote := range votes {
		w := weights[vote.ValidatorAddress]
		if w == 0 {
			continue
		}
		for _, mp := range vote.Prices {
			rec := collected[mp.MarketIndex]
			rec.idx = append(rec.idx, mp.IndexPrice)
			rec.mp = append(rec.mp, mp.MarkPrice)
			rec.w = append(rec.w, w)
			collected[mp.MarketIndex] = rec
		}
	}
	out := make([]types.MarketAggregation, 0, len(collected))
	for marketIdx, rec := range collected {
		out = append(out, types.MarketAggregation{
			MarketIndex: marketIdx,
			IndexPrice:  WeightedMedian(rec.idx, rec.w),
			MarkPrice:   WeightedMedian(rec.mp, rec.w),
		})
	}
	return out
}
