package keeper

import (
	"context"

	abci "github.com/cometbft/cometbft/abci/types"
	cmtproto "github.com/cometbft/cometbft/proto/tendermint/types"

	sdk "github.com/cosmos/cosmos-sdk/types"

	"github.com/perpdex/perpdex-l1/x/oracle/types"
)

// PreBlocker decodes the proposer-injected ExtendedCommitInfo bundle from
// `Txs[0]` of the finalised block, runs a stake-weighted median per
// market over every committed vote-extension, and writes the result to
// state via Keeper.SetPrice.
//
// It is registered against `app.SetPreBlocker(...)` AFTER the module
// manager's PreBlock so module-level upgrades have already been applied.
//
// The PreBlocker is robust to malformed input: if Txs[0] cannot be
// decoded or the bundle fails the supermajority check, the function
// returns nil and skips the price update for this block. This is
// preferable to halting the chain — the chain will simply produce a
// block with no oracle update, exactly as if every validator had voted
// an empty extension.
func (h *VoteExtensionHandler) PreBlocker() sdk.PreBlocker {
	return func(ctx sdk.Context, req *abci.RequestFinalizeBlock) (*sdk.ResponsePreBlock, error) {
		if !h.veEnabled(ctx, req.Height) {
			return &sdk.ResponsePreBlock{}, nil
		}
		if len(req.Txs) < NumInjectedTxs {
			return &sdk.ResponsePreBlock{}, nil
		}
		extInfo, err := h.ecCodec.Decode(req.Txs[OracleInfoIndex])
		if err != nil {
			ctx.Logger().Debug("oracle preblock: decode extended commit", "err", err)
			return &sdk.ResponsePreBlock{}, nil
		}
		params, err := h.keeper.Params.Get(ctx)
		if err != nil {
			return &sdk.ResponsePreBlock{}, nil
		}
		aggregations := h.aggregateFromExtInfo(extInfo, params)
		if len(aggregations) == 0 {
			return &sdk.ResponsePreBlock{}, nil
		}
		now := ctx.BlockTime().UnixMilli()
		for _, agg := range aggregations {
			p := types.OraclePrice{
				MarketIndex:          agg.marketIndex,
				IndexPrice:           agg.indexPrice,
				MarkPrice:            agg.markPrice,
				LastUpdatedTimestamp: now,
				LastUpdatedHeight:    req.Height,
				ParticipantCount:     agg.participantCount,
				TotalVotingPower:     agg.totalPower,
			}
			h.applyMarkSmoothing(ctx, &p, params)
			if err := h.keeper.SetPrice(ctx, p); err != nil {
				ctx.Logger().Error("oracle preblock: set price", "market", agg.marketIndex, "err", err)
				continue
			}
		}
		return &sdk.ResponsePreBlock{}, nil
	}
}

// aggregation is the per-market output of the PreBlocker's weighted-median
// pass. It is private because no caller outside the keeper consumes this
// shape; it would be promoted to a public struct if the proposer ever
// needed to attest to the same numbers.
type aggregation struct {
	marketIndex      uint32
	indexPrice       uint32
	markPrice        uint32
	participantCount uint32
	totalPower       uint64
}

// aggregateFromExtInfo runs the same weighted median per market that the
// proposer would have run in PrepareProposal. We do it again here on every
// validator so the chain's price update never trusts the proposer's
// arithmetic — only the cometbft 2/3+ commit on the ExtendedCommitInfo
// shape itself.
//
// Quorum and outlier-rejection thresholds come from `params`:
//   - `MinVotingPowerRatio`: bps fraction of the total committed power that
//     must back a market's votes for the median to be accepted.
//   - `DeviationThresholdBps`: bps band around a first-pass median;
//     samples outside the band are dropped and the median is recomputed.
func (h *VoteExtensionHandler) aggregateFromExtInfo(
	ext abci.ExtendedCommitInfo,
	params types.Params,
) []aggregation {
	var totalCommittedPower int64
	committed := make([]abci.ExtendedVoteInfo, 0, len(ext.Votes))
	for _, v := range ext.Votes {
		if v.BlockIdFlag != cmtproto.BlockIDFlagCommit {
			continue
		}
		totalCommittedPower += v.Validator.Power
		committed = append(committed, v)
	}
	if totalCommittedPower <= 0 || len(committed) == 0 {
		return nil
	}

	type sample struct {
		index  uint32
		mark   uint32
		weight uint64
	}
	collected := make(map[uint32][]sample)
	for _, v := range committed {
		if len(v.VoteExtension) == 0 {
			continue
		}
		ov, err := h.veCodec.Decode(v.VoteExtension)
		if err != nil {
			continue
		}
		// SubmittedAtHeight is informational only; cometbft already
		// commits to the height implicitly via the ExtendedCommitInfo
		// it provides on the next block, so we don't block aggregation
		// on a mismatch here.
		_ = ov.SubmittedAtHeight
		w := uint64(v.Validator.Power)
		if w == 0 {
			continue
		}
		for _, mp := range ov.Prices {
			if mp.IndexPrice == 0 || mp.MarkPrice == 0 {
				continue
			}
			collected[mp.MarketIndex] = append(collected[mp.MarketIndex], sample{
				index:  mp.IndexPrice,
				mark:   mp.MarkPrice,
				weight: w,
			})
		}
	}
	if len(collected) == 0 {
		return nil
	}

	out := make([]aggregation, 0, len(collected))
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
			if marketPower*10_000 < uint64(totalCommittedPower)*quorumNumerator {
				continue
			}
		}
		idx := WeightedMedian(idxVals, idxWeights)
		mk := WeightedMedian(mkVals, mkWeights)
		if idx == 0 || mk == 0 {
			continue
		}
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
		out = append(out, aggregation{
			marketIndex:      marketIdx,
			indexPrice:       idx,
			markPrice:        mk,
			participantCount: uint32(len(samples)),
			totalPower:       marketPower,
		})
	}
	return out
}

// applyMarkSmoothing applies the Params.MarkPriceEmaAlpha-driven EMA on
// the freshly aggregated mark price.
func (h *VoteExtensionHandler) applyMarkSmoothing(ctx context.Context, p *types.OraclePrice, params types.Params) {
	alpha := params.MarkPriceEmaAlpha
	if alpha == 0 || alpha >= 10_000 {
		return
	}
	prev, err := h.keeper.GetPrice(ctx, p.MarketIndex)
	if err != nil || prev.MarkPrice == 0 {
		return
	}
	smoothed := (uint64(alpha)*uint64(p.MarkPrice) + uint64(10_000-alpha)*uint64(prev.MarkPrice)) / 10_000
	if smoothed == 0 {
		smoothed = 1
	}
	p.MarkPrice = uint32(smoothed)
}

// withinBps reports whether `value` is within `thresholdBps` basis points
// of `reference`. Used to drop outlier samples before re-medianing.
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
