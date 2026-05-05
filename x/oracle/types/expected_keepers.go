package types

import (
	"context"

	"cosmossdk.io/math"

	sdk "github.com/cosmos/cosmos-sdk/types"
	stakingtypes "github.com/cosmos/cosmos-sdk/x/staking/types"
)

// StakingKeeper is the subset required by the vote-extension pipeline:
// PrepareProposal looks up each voter's voting power by consensus address
// and the total bonded power for quorum calculation.
type StakingKeeper interface {
	GetValidatorByConsAddr(ctx context.Context, consAddr sdk.ConsAddress) (stakingtypes.Validator, error)
	TotalBondedTokens(ctx context.Context) (math.Int, error)
	BondDenom(ctx context.Context) (string, error)
	PowerReduction(ctx context.Context) math.Int
}
