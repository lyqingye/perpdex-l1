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

// MarketShim is the minimal market description the oracle daemon needs to
// build its currency-pair → market-index resolver. The full Market lives in
// x/market/types but pulling that whole package in from the daemon would be
// circular at app wiring time, so we expose a copy here.
type MarketShim struct {
	MarketIndex  uint32
	BaseAssetID  uint32
	QuoteAssetID uint32
	// Decimals is the number of decimal digits the chain uses to encode the
	// integer price for this market (e.g. Decimals=2 means price 12345 means
	// 123.45). Zero means "use the daemon default".
	Decimals uint8
}
