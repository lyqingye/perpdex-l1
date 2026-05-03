package types

import (
	"context"

	"cosmossdk.io/math"

	sdk "github.com/cosmos/cosmos-sdk/types"
	stakingtypes "github.com/cosmos/cosmos-sdk/x/staking/types"
)

// StakingKeeper is the subset required to enumerate validator voting power
// during PoS oracle aggregation.
type StakingKeeper interface {
	IterateBondedValidatorsByPower(ctx context.Context, fn func(index int64, validator stakingtypes.ValidatorI) (stop bool)) error
	GetValidator(ctx context.Context, addr sdk.ValAddress) (stakingtypes.Validator, error)
	BondDenom(ctx context.Context) (string, error)
}

// SlashingKeeper is the subset used by oracle to penalize misbehaviour.
type SlashingKeeper interface {
	JailUntil(ctx context.Context, consAddr sdk.ConsAddress, jailTime int64) error
	Jail(ctx context.Context, consAddr sdk.ConsAddress) error
	Slash(ctx context.Context, consAddr sdk.ConsAddress, fraction math.LegacyDec, power int64, distributionHeight int64) error
}
