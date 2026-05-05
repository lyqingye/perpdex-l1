package keeper

import (
	"bytes"
	"fmt"

	abci "github.com/cometbft/cometbft/abci/types"
	cmtproto "github.com/cometbft/cometbft/proto/tendermint/types"

	"github.com/cosmos/cosmos-sdk/client"
	sdk "github.com/cosmos/cosmos-sdk/types"

	"github.com/perpdex/perpdex-l1/x/oracle/types"
)

// VoteExtensionHandler bundles the four ABCI++ handlers required by the
// dydx/Slinky-style oracle pipeline. They are wired in app.go via
// `bApp.SetExtendVoteHandler(...)` etc. and become no-ops when
// `Params.VoteExtensionEnabled` is false (or before the consensus-level
// `VoteExtensionsEnableHeight`).
//
// Pipeline summary:
//
//  1. ExtendVote: each validator's local node calls PriceFetcher and signs
//     the resulting OracleVote payload as part of its prevote.
//  2. VerifyVoteExtension: peers stateless-validate the payload (decode +
//     non-zero prices + height match). Cryptographic signature verification
//     of the extension itself is performed by cometbft / baseapp.
//  3. PrepareProposal: the proposer walks the previous block's
//     ExtendedCommitInfo (req.LocalLastCommit), decodes every commit-flag
//     vote's extension, runs a voting-power-weighted median per market,
//     wraps the result in MsgAggregateOracleVotes and prepends it as the
//     first transaction of the new block.
//  4. ProcessProposal: every other validator structurally verifies that
//     the first tx is the proposer-injected MsgAggregateOracleVotes
//     signed by the gov authority. Cryptographic re-derivation against
//     the previous block's vote extensions is left as a follow-up because
//     RequestProcessProposal does not carry ExtendedCommitInfo by default;
//     until that is wired the pipeline relies on the cometbft 2/3-honest
//     assumption for a single block of price freshness, identical to the
//     trust model dydx ran with before the slinky-2 hardening.
type VoteExtensionHandler struct {
	keeper    Keeper
	txConfig  client.TxConfig
	govAuthor string
}

// NewVoteExtensionHandler constructs a handler bundle. `govAuthor` is the
// bech32-encoded gov module account address used as the signer of the
// proposer-injected MsgAggregateOracleVotes.
func NewVoteExtensionHandler(k Keeper, txConfig client.TxConfig, govAuthor string) *VoteExtensionHandler {
	return &VoteExtensionHandler{keeper: k, txConfig: txConfig, govAuthor: govAuthor}
}

// veEnabled returns true when both the on-chain Params toggle and the
// cometbft consensus param agree that vote-extensions are active for the
// current height. We check both because the on-chain switch is a soft
// kill-switch for this module while the consensus param is the immutable
// source of truth on whether the network is delivering VEs at all.
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

// ExtendVote returns the local oracle operator's price set as the vote
// extension. With vote_extension_enabled=false (or before the enable
// height) it returns an empty payload so consensus is never blocked.
func (h *VoteExtensionHandler) ExtendVote() sdk.ExtendVoteHandler {
	return func(ctx sdk.Context, req *abci.RequestExtendVote) (*abci.ResponseExtendVote, error) {
		if !h.veEnabled(ctx, req.Height) {
			return &abci.ResponseExtendVote{}, nil
		}
		fetcher := h.keeper.PriceFetcher()
		prices, err := fetcher.FetchPrices(ctx, req.Height)
		if err != nil {
			// Returning an error here would crash the validator; instead
			// emit an empty payload so peers see "no opinion" and the
			// proposer can still produce a block.
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
		bz, err := vote.Marshal()
		if err != nil {
			return &abci.ResponseExtendVote{}, nil
		}
		return &abci.ResponseExtendVote{VoteExtension: bz}, nil
	}
}

// VerifyVoteExtension performs stateless validation of a peer's payload:
// it must decode, advertise the same height, and contain only non-zero
// prices.
func (h *VoteExtensionHandler) VerifyVoteExtension() sdk.VerifyVoteExtensionHandler {
	return func(ctx sdk.Context, req *abci.RequestVerifyVoteExtension) (*abci.ResponseVerifyVoteExtension, error) {
		if !h.veEnabled(ctx, req.Height) {
			if len(req.VoteExtension) > 0 {
				return &abci.ResponseVerifyVoteExtension{Status: abci.ResponseVerifyVoteExtension_REJECT}, nil
			}
			return &abci.ResponseVerifyVoteExtension{Status: abci.ResponseVerifyVoteExtension_ACCEPT}, nil
		}
		if len(req.VoteExtension) == 0 {
			// Validators are allowed to sit out a single block (no opinion).
			return &abci.ResponseVerifyVoteExtension{Status: abci.ResponseVerifyVoteExtension_ACCEPT}, nil
		}
		var v types.OracleVote
		if err := v.Unmarshal(req.VoteExtension); err != nil {
			return &abci.ResponseVerifyVoteExtension{Status: abci.ResponseVerifyVoteExtension_REJECT}, nil
		}
		if v.SubmittedAtHeight != req.Height {
			return &abci.ResponseVerifyVoteExtension{Status: abci.ResponseVerifyVoteExtension_REJECT}, nil
		}
		seen := map[uint32]struct{}{}
		for _, mp := range v.Prices {
			if mp.IndexPrice == 0 || mp.MarkPrice == 0 {
				return &abci.ResponseVerifyVoteExtension{Status: abci.ResponseVerifyVoteExtension_REJECT}, nil
			}
			if _, dup := seen[mp.MarketIndex]; dup {
				return &abci.ResponseVerifyVoteExtension{Status: abci.ResponseVerifyVoteExtension_REJECT}, nil
			}
			seen[mp.MarketIndex] = struct{}{}
		}
		return &abci.ResponseVerifyVoteExtension{Status: abci.ResponseVerifyVoteExtension_ACCEPT}, nil
	}
}

// PrepareProposal aggregates the previous block's vote extensions and
// prepends the resulting MsgAggregateOracleVotes as the first transaction
// of the new block.
func (h *VoteExtensionHandler) PrepareProposal(defaultHandler sdk.PrepareProposalHandler) sdk.PrepareProposalHandler {
	return func(ctx sdk.Context, req *abci.RequestPrepareProposal) (*abci.ResponsePrepareProposal, error) {
		if !h.veEnabled(ctx, req.Height) {
			return defaultHandler(ctx, req)
		}
		injectedTxBz, err := h.buildInjectedTx(ctx, req)
		if err != nil || len(injectedTxBz) == 0 {
			// Aggregation failed (no quorum, codec error, etc.). Fall
			// through to the default handler so block production keeps
			// running with stale prices instead of stalling consensus.
			return defaultHandler(ctx, req)
		}
		// Reserve room for the injected tx so the wrapped handler does
		// not over-fill the block.
		injectedSize := int64(len(injectedTxBz))
		if injectedSize >= req.MaxTxBytes {
			return defaultHandler(ctx, req)
		}
		req.MaxTxBytes -= injectedSize
		resp, err := defaultHandler(ctx, req)
		if err != nil {
			return nil, err
		}
		resp.Txs = injectAndResize(resp.Txs, injectedTxBz, req.MaxTxBytes+injectedSize)
		return resp, nil
	}
}

// ProcessProposal verifies the proposer-injected MsgAggregateOracleVotes
// before deferring to the default handler for the rest of the block.
func (h *VoteExtensionHandler) ProcessProposal(defaultHandler sdk.ProcessProposalHandler) sdk.ProcessProposalHandler {
	return func(ctx sdk.Context, req *abci.RequestProcessProposal) (*abci.ResponseProcessProposal, error) {
		if !h.veEnabled(ctx, req.Height) {
			return defaultHandler(ctx, req)
		}
		if len(req.Txs) == 0 {
			return &abci.ResponseProcessProposal{Status: abci.ResponseProcessProposal_REJECT}, nil
		}
		tx, err := h.txConfig.TxDecoder()(req.Txs[0])
		if err != nil {
			return &abci.ResponseProcessProposal{Status: abci.ResponseProcessProposal_REJECT}, nil
		}
		msgs := tx.GetMsgs()
		if len(msgs) != 1 {
			return &abci.ResponseProcessProposal{Status: abci.ResponseProcessProposal_REJECT}, nil
		}
		agg, ok := msgs[0].(*types.MsgAggregateOracleVotes)
		if !ok {
			return &abci.ResponseProcessProposal{Status: abci.ResponseProcessProposal_REJECT}, nil
		}
		if agg.Authority != h.govAuthor {
			return &abci.ResponseProcessProposal{Status: abci.ResponseProcessProposal_REJECT}, nil
		}
		if agg.Height != req.Height {
			return &abci.ResponseProcessProposal{Status: abci.ResponseProcessProposal_REJECT}, nil
		}
		if err := agg.ValidateBasic(); err != nil {
			return &abci.ResponseProcessProposal{Status: abci.ResponseProcessProposal_REJECT}, nil
		}
		return defaultHandler(ctx, req)
	}
}

// buildInjectedTx walks the previous-block ExtendedCommitInfo, decodes
// every commit-flag vote extension as OracleVote, runs a voting-power
// weighted median per market and returns the encoded
// MsgAggregateOracleVotes transaction. Returns an empty slice (no error)
// when no market hits the configured quorum.
func (h *VoteExtensionHandler) buildInjectedTx(ctx sdk.Context, req *abci.RequestPrepareProposal) ([]byte, error) {
	params, err := h.keeper.Params.Get(ctx)
	if err != nil {
		return nil, err
	}

	votes := []types.OracleVote{}
	weights := []uint64{}
	totalCommittedPower := int64(0)
	for _, vote := range req.LocalLastCommit.Votes {
		if vote.BlockIdFlag != cmtproto.BlockIDFlagCommit {
			continue
		}
		totalCommittedPower += vote.Validator.Power
		if len(vote.VoteExtension) == 0 {
			continue
		}
		var ov types.OracleVote
		if err := ov.Unmarshal(vote.VoteExtension); err != nil {
			continue
		}
		votes = append(votes, ov)
		weights = append(weights, uint64(vote.Validator.Power))
	}
	if totalCommittedPower == 0 || len(votes) == 0 {
		return nil, nil
	}

	aggregations := h.aggregate(votes, weights, totalCommittedPower, params)
	if len(aggregations) == 0 {
		return nil, nil
	}
	msg := &types.MsgAggregateOracleVotes{
		Authority:    h.govAuthor,
		Height:       req.Height,
		Aggregations: aggregations,
	}
	return h.encodeInjectedTx(msg)
}

// aggregate runs the voting-power weighted median per market while honouring
// the quorum (`min_voting_power_ratio`) and outlier (`deviation_threshold_bps`)
// guards from Params.
func (h *VoteExtensionHandler) aggregate(votes []types.OracleVote, weights []uint64, totalCommittedPower int64, params types.Params) []types.MarketAggregation {
	type sample struct {
		index  uint32
		mark   uint32
		weight uint64
	}
	collected := map[uint32][]sample{}
	for i, v := range votes {
		w := weights[i]
		if w == 0 {
			continue
		}
		for _, mp := range v.Prices {
			collected[mp.MarketIndex] = append(collected[mp.MarketIndex], sample{
				index:  mp.IndexPrice,
				mark:   mp.MarkPrice,
				weight: w,
			})
		}
	}
	out := make([]types.MarketAggregation, 0, len(collected))
	quorumNumerator := uint64(params.MinVotingPowerRatio)
	for marketIdx, samples := range collected {
		var marketPower uint64
		idxVals := make([]uint32, 0, len(samples))
		idxWeights := make([]uint64, 0, len(samples))
		mkVals := make([]uint32, 0, len(samples))
		mkWeights := make([]uint64, 0, len(samples))
		for _, s := range samples {
			marketPower += s.weight
			idxVals = append(idxVals, s.index)
			idxWeights = append(idxWeights, s.weight)
			mkVals = append(mkVals, s.mark)
			mkWeights = append(mkWeights, s.weight)
		}
		if quorumNumerator > 0 && uint64(totalCommittedPower) > 0 {
			// quorum check: marketPower * 10000 >= total * minRatioBps
			if marketPower*10_000 < uint64(totalCommittedPower)*quorumNumerator {
				continue
			}
		}
		idx := WeightedMedian(idxVals, idxWeights)
		mk := WeightedMedian(mkVals, mkWeights)
		if idx == 0 || mk == 0 {
			continue
		}
		// Optional outlier rejection: drop samples beyond
		// `deviation_threshold_bps` of the median, then re-run.
		if params.DeviationThresholdBps > 0 {
			idxVals2 := make([]uint32, 0, len(samples))
			idxWeights2 := make([]uint64, 0, len(samples))
			mkVals2 := make([]uint32, 0, len(samples))
			mkWeights2 := make([]uint64, 0, len(samples))
			for j, v := range idxVals {
				if !withinBps(v, idx, params.DeviationThresholdBps) {
					continue
				}
				if !withinBps(mkVals[j], mk, params.DeviationThresholdBps) {
					continue
				}
				idxVals2 = append(idxVals2, v)
				idxWeights2 = append(idxWeights2, idxWeights[j])
				mkVals2 = append(mkVals2, mkVals[j])
				mkWeights2 = append(mkWeights2, mkWeights[j])
			}
			if len(idxVals2) > 0 {
				idx = WeightedMedian(idxVals2, idxWeights2)
				mk = WeightedMedian(mkVals2, mkWeights2)
			}
			if idx == 0 || mk == 0 {
				continue
			}
		}
		out = append(out, types.MarketAggregation{
			MarketIndex: marketIdx,
			IndexPrice:  idx,
			MarkPrice:   mk,
		})
	}
	return out
}

// encodeInjectedTx wraps MsgAggregateOracleVotes in a Cosmos transaction
// with empty signatures. The chain's `OracleInjectedTxDecorator` ante
// recognises this proposer-injected pattern and routes it past the
// signature-verification decorators.
func (h *VoteExtensionHandler) encodeInjectedTx(msg *types.MsgAggregateOracleVotes) ([]byte, error) {
	builder := h.txConfig.NewTxBuilder()
	if err := builder.SetMsgs(msg); err != nil {
		return nil, fmt.Errorf("oracle: encode injected tx: %w", err)
	}
	bz, err := h.txConfig.TxEncoder()(builder.GetTx())
	if err != nil {
		return nil, fmt.Errorf("oracle: encode injected tx: %w", err)
	}
	return bz, nil
}

// injectAndResize prepends `injectTx` to `appTxs` while keeping the total
// size <= maxSizeBytes. Mirrors slinky's idempotent helper.
func injectAndResize(appTxs [][]byte, injectTx []byte, maxSizeBytes int64) [][]byte {
	var (
		returned       [][]byte
		consumedBytes  int64
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

// withinBps reports whether `value` is within `thresholdBps` basis points of
// `reference`. Used to drop outlier samples before re-medianing.
func withinBps(value, reference uint32, thresholdBps uint32) bool {
	if reference == 0 {
		return true
	}
	var diff uint64
	if value > reference {
		diff = uint64(value - reference)
	} else {
		diff = uint64(reference - value)
	}
	return diff*10_000 <= uint64(reference)*uint64(thresholdBps)
}
