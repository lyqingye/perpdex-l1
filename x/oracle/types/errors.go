package types

import "cosmossdk.io/errors"

var (
	ErrInvalidAuthority    = errors.Register(ModuleName, 2, "invalid authority")
	ErrPriceNotFound       = errors.Register(ModuleName, 3, "oracle price not found")
	ErrInvalidParams       = errors.Register(ModuleName, 9, "invalid params")
	ErrInvalidPrice        = errors.Register(ModuleName, 11, "invalid price")
	ErrInvalidVote         = errors.Register(ModuleName, 12, "invalid oracle vote")
	ErrStalePrice          = errors.Register(ModuleName, 13, "oracle price is stale")
	ErrVoteExtDisabled     = errors.Register(ModuleName, 14, "vote-extension oracle pipeline is disabled")
	ErrAggregationMismatch = errors.Register(ModuleName, 15, "proposer aggregation does not match vote-extension re-derivation")
	ErrInjectedTxBlocked   = errors.Register(ModuleName, 16, "MsgAggregateOracleVotes can only be proposer-injected")
)
