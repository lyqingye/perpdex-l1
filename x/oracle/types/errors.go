package types

import "cosmossdk.io/errors"

var (
	ErrInvalidAuthority = errors.Register(ModuleName, 2, "invalid authority")
	ErrPriceNotFound    = errors.Register(ModuleName, 3, "oracle price not found")
	ErrInvalidParams    = errors.Register(ModuleName, 9, "invalid params")
	ErrInvalidPrice     = errors.Register(ModuleName, 11, "invalid price")
	ErrInvalidVote      = errors.Register(ModuleName, 12, "invalid oracle vote")
	ErrStalePrice       = errors.Register(ModuleName, 13, "oracle price is stale")
	ErrVoteExtDisabled  = errors.Register(ModuleName, 14, "vote-extension oracle pipeline is disabled")
	// ErrInsufficientVotingPower is raised by ProcessProposal when the
	// supplied ExtendedCommitInfo lacks the 2/3+ voting-power supermajority
	// dydx/Connect require for a valid commit.
	ErrInsufficientVotingPower = errors.Register(ModuleName, 17, "insufficient voting power for vote extensions")
	// ErrMissingCommitInfo is raised by ProcessProposal when Txs[0] is not
	// the proposer-injected ExtendedCommitInfo bytes.
	ErrMissingCommitInfo = errors.Register(ModuleName, 18, "missing or invalid commit info in proposal Txs[0]")
)
