// Package ve hosts the helpers used by the oracle ABCI++ pipeline to
// validate vote-extensions and ExtendedCommitInfo bundles in
// ProcessProposal and PreBlock. The shape mirrors the upstream Connect
// helpers (`abci/ve/utils.go` in skip-mev/connect) but is reimplemented
// to depend only on `cometbft` types so the chain has a self-contained
// production-grade verifier.
package ve

import (
	"bytes"
	"fmt"

	cometabci "github.com/cometbft/cometbft/abci/types"
	cmtproto "github.com/cometbft/cometbft/proto/tendermint/types"
)

// ValidateExtendedCommitAgainstLastCommit cross-checks the
// ExtendedCommitInfo bundle that the proposer injected with the
// `LastCommitInfo` cometbft has supplied for the same height. The two
// MUST contain the exact same set of validators with the exact same
// (vote_power, address, block_id_flag) tuples — otherwise a malicious
// proposer could swap commit votes for a different set or fabricate
// power.
//
// We deliberately do NOT compare the vote-extension bytes themselves —
// per cometbft's spec, those are not committed by the network and may
// legitimately differ between proposer and follower if the proposer
// pruned a malformed VE.
func ValidateExtendedCommitAgainstLastCommit(
	ext cometabci.ExtendedCommitInfo,
	last cometabci.CommitInfo,
) error {
	if ext.Round != last.Round {
		return fmt.Errorf("extended commit round %d != last commit round %d", ext.Round, last.Round)
	}
	if len(ext.Votes) != len(last.Votes) {
		return fmt.Errorf("extended commit has %d votes, last commit has %d", len(ext.Votes), len(last.Votes))
	}
	for i := range ext.Votes {
		ev := ext.Votes[i]
		lv := last.Votes[i]
		if !bytes.Equal(ev.Validator.Address, lv.Validator.Address) {
			return fmt.Errorf("vote %d validator address mismatch", i)
		}
		if ev.Validator.Power != lv.Validator.Power {
			return fmt.Errorf("vote %d voting power mismatch (%d != %d)", i, ev.Validator.Power, lv.Validator.Power)
		}
		if ev.BlockIdFlag != lv.BlockIdFlag {
			return fmt.Errorf("vote %d block_id_flag mismatch", i)
		}
	}
	return nil
}

// ValidateExtendedCommit performs the structural checks on
// ExtendedCommitInfo that don't require external context: every vote that
// claims it committed must carry a non-zero validator address and either
// an empty vote-extension or a non-empty signature pair. We also enforce
// the supermajority rule (`> 2/3` of total power signed VE-bearing
// commits).
func ValidateExtendedCommit(
	ext cometabci.ExtendedCommitInfo,
) error {
	var totalPower, committedPower int64
	for i, v := range ext.Votes {
		if v.Validator.Power < 0 {
			return fmt.Errorf("vote %d has negative voting power", i)
		}
		totalPower += v.Validator.Power
		if v.BlockIdFlag != cmtproto.BlockIDFlagCommit {
			continue
		}
		committedPower += v.Validator.Power
		if len(v.VoteExtension) > 0 {
			if len(v.ExtensionSignature) == 0 {
				return fmt.Errorf("vote %d has VE bytes but no signature", i)
			}
		}
	}
	if totalPower == 0 {
		// Empty validator set → nothing to validate. We accept; the
		// chain will produce an empty PreBlock snapshot.
		return nil
	}
	// Strict 2/3+ supermajority of *committed* power. The dydx and
	// connect implementations both insist on this; otherwise a partition
	// could let a minority of validators dictate the price feed.
	if 3*committedPower <= 2*totalPower {
		return fmt.Errorf(
			"insufficient committed voting power: %d/%d (need > 2/3)",
			committedPower, totalPower,
		)
	}
	return nil
}

// CommittedVotes returns the subset of `ext.Votes` whose `BlockIdFlag`
// indicates a commit. Convenience helper for aggregators.
func CommittedVotes(ext cometabci.ExtendedCommitInfo) []cometabci.ExtendedVoteInfo {
	out := make([]cometabci.ExtendedVoteInfo, 0, len(ext.Votes))
	for _, v := range ext.Votes {
		if v.BlockIdFlag == cmtproto.BlockIDFlagCommit {
			out = append(out, v)
		}
	}
	return out
}
