// Suite: ABCI++ ExtendedCommitInfo structural validation.
//
// These cases protect the supermajority + signature invariants that
// `ve.ValidateExtendedCommit` and `ve.ValidateExtendedCommitAgainstLastCommit`
// enforce on the receiver side of the VE pipeline. A regression here
// could let a partition (or a malicious proposer) inject vote
// extensions that no longer reflect the committed validator set.
package tests

import (
	"testing"

	cometabci "github.com/cometbft/cometbft/abci/types"
	cmtproto "github.com/cometbft/cometbft/proto/tendermint/types"
	"github.com/stretchr/testify/require"

	"github.com/perpdex/perpdex-l1/x/oracle/abci/ve"
)

func TestValidateExtendedCommitSupermajority(t *testing.T) {
	ec := cometabci.ExtendedCommitInfo{
		Votes: []cometabci.ExtendedVoteInfo{
			{Validator: cometabci.Validator{Address: []byte("a"), Power: 67}, BlockIdFlag: cmtproto.BlockIDFlagCommit, VoteExtension: []byte("ve"), ExtensionSignature: []byte("sig")},
			{Validator: cometabci.Validator{Address: []byte("b"), Power: 33}, BlockIdFlag: cmtproto.BlockIDFlagAbsent},
		},
	}
	require.NoError(t, ve.ValidateExtendedCommit(ec))
}

func TestValidateExtendedCommitMinority(t *testing.T) {
	ec := cometabci.ExtendedCommitInfo{
		Votes: []cometabci.ExtendedVoteInfo{
			{Validator: cometabci.Validator{Address: []byte("a"), Power: 30}, BlockIdFlag: cmtproto.BlockIDFlagCommit},
			{Validator: cometabci.Validator{Address: []byte("b"), Power: 70}, BlockIdFlag: cmtproto.BlockIDFlagAbsent},
		},
	}
	require.Error(t, ve.ValidateExtendedCommit(ec))
}

func TestValidateExtendedCommitVEMustBeSigned(t *testing.T) {
	ec := cometabci.ExtendedCommitInfo{
		Votes: []cometabci.ExtendedVoteInfo{
			{Validator: cometabci.Validator{Address: []byte("a"), Power: 100}, BlockIdFlag: cmtproto.BlockIDFlagCommit, VoteExtension: []byte("ve")},
		},
	}
	require.Error(t, ve.ValidateExtendedCommit(ec))
}

func TestValidateExtendedCommitAgainstLastCommit(t *testing.T) {
	ext := cometabci.ExtendedCommitInfo{
		Round: 0,
		Votes: []cometabci.ExtendedVoteInfo{
			{Validator: cometabci.Validator{Address: []byte("a"), Power: 100}, BlockIdFlag: cmtproto.BlockIDFlagCommit},
		},
	}
	last := cometabci.CommitInfo{
		Round: 0,
		Votes: []cometabci.VoteInfo{
			{Validator: cometabci.Validator{Address: []byte("a"), Power: 100}, BlockIdFlag: cmtproto.BlockIDFlagCommit},
		},
	}
	require.NoError(t, ve.ValidateExtendedCommitAgainstLastCommit(ext, last))

	last.Votes[0].Validator.Power = 99
	require.Error(t, ve.ValidateExtendedCommitAgainstLastCommit(ext, last))
}

func TestCommittedVotes(t *testing.T) {
	ec := cometabci.ExtendedCommitInfo{
		Votes: []cometabci.ExtendedVoteInfo{
			{BlockIdFlag: cmtproto.BlockIDFlagCommit},
			{BlockIdFlag: cmtproto.BlockIDFlagAbsent},
			{BlockIdFlag: cmtproto.BlockIDFlagCommit},
		},
	}
	require.Len(t, ve.CommittedVotes(ec), 2)
}
