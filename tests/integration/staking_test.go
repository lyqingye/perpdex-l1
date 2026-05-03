package integration

import (
	"testing"

	"github.com/stretchr/testify/require"

	"cosmossdk.io/math"

	"github.com/cosmos/cosmos-sdk/testutil/testdata"
	sdk "github.com/cosmos/cosmos-sdk/types"
	authtypes "github.com/cosmos/cosmos-sdk/x/auth/types"
	minttypes "github.com/cosmos/cosmos-sdk/x/mint/types"
	stakingtypes "github.com/cosmos/cosmos-sdk/x/staking/types"

	perptypes "github.com/perpdex/perpdex-l1/types"
)

// TestStaking_DelegateAndQuery walks through:
//  1. Funding a delegator from the bank module.
//  2. Bootstrapping a single validator into the staking module.
//  3. Delegating to that validator and asserting the delegation is stored
//     correctly.
//
// It serves as a template for any future cross-module integration test.
func TestStaking_DelegateAndQuery(t *testing.T) {
	f := initFixture(t)

	bondDenom := perptypes.UPerpDenom
	stakingParams, err := f.stakingKeeper.GetParams(f.sdkCtx)
	require.NoError(t, err)
	stakingParams.BondDenom = bondDenom
	require.NoError(t, f.stakingKeeper.SetParams(f.sdkCtx, stakingParams))

	// Build a validator with a freshly generated consensus key.
	valPrivKey, valPubKey, valConsAddr := testdata.KeyTestPubAddr()
	_ = valPrivKey
	valOperator := sdk.ValAddress(valConsAddr)

	bondAmt := sdk.DefaultPowerReduction
	startBal := sdk.NewCoins(sdk.NewCoin(bondDenom, bondAmt.MulRaw(10)))

	// Fund the delegator (which also acts as the validator's operator) by
	// minting through the mint module account (which has the Minter perm).
	require.NoError(t, f.bankKeeper.MintCoins(f.sdkCtx, minttypes.ModuleName, startBal))
	require.NoError(t, f.bankKeeper.SendCoinsFromModuleToAccount(
		f.sdkCtx, minttypes.ModuleName, sdk.AccAddress(valOperator), startBal,
	))

	// Create an unbonded validator and let staking keeper handle bond pool moves.
	validator, err := stakingtypes.NewValidator(valOperator.String(), valPubKey, stakingtypes.Description{Moniker: "val"})
	require.NoError(t, err)
	validator.MinSelfDelegation = math.OneInt()
	validator.Commission = stakingtypes.NewCommission(math.LegacyZeroDec(), math.LegacyZeroDec(), math.LegacyZeroDec())
	require.NoError(t, f.stakingKeeper.SetValidator(f.sdkCtx, validator))
	require.NoError(t, f.stakingKeeper.SetValidatorByConsAddr(f.sdkCtx, validator))
	require.NoError(t, f.stakingKeeper.SetNewValidatorByPowerIndex(f.sdkCtx, validator))
	require.NoError(t, f.stakingKeeper.Hooks().AfterValidatorCreated(f.sdkCtx, valOperator))

	require.NoError(t,
		delegateCoinsFromAccount(f.sdkCtx, f.stakingKeeper, sdk.AccAddress(valOperator), bondAmt, validator),
	)

	// staking keeper should now report a delegation of `bondAmt`.
	delegation, err := f.stakingKeeper.GetDelegation(f.sdkCtx, sdk.AccAddress(valOperator), valOperator)
	require.NoError(t, err)
	require.True(t, delegation.Shares.GT(math.LegacyZeroDec()))

	// Validator is still Unbonded at this point, so the staked coins live in
	// the not-bonded pool.
	notBondedPool := authtypes.NewModuleAddress(stakingtypes.NotBondedPoolName)
	require.Equal(t, bondAmt.String(),
		f.bankKeeper.GetBalance(f.sdkCtx, notBondedPool, bondDenom).Amount.String(),
	)

	// Apply pending validator updates: the validator should be promoted to
	// the active set and its tokens moved into the bonded pool.
	applyValidatorSetUpdates(t, f.sdkCtx, f.stakingKeeper, 1)

	bondedPool := authtypes.NewModuleAddress(stakingtypes.BondedPoolName)
	require.Equal(t, bondAmt.String(),
		f.bankKeeper.GetBalance(f.sdkCtx, bondedPool, bondDenom).Amount.String(),
	)
	require.Equal(t, "0",
		f.bankKeeper.GetBalance(f.sdkCtx, notBondedPool, bondDenom).Amount.String(),
	)
}

// TestBank_SendBetweenAccounts is a minimal sanity check that the bank
// keeper inside the integration app honors basic transfers between two
// accounts.
func TestBank_SendBetweenAccounts(t *testing.T) {
	f := initFixture(t)

	denom := perptypes.UPerpDenom
	mintAmt := sdk.NewCoins(sdk.NewCoin(denom, math.NewInt(1_000_000)))
	sendAmt := sdk.NewCoins(sdk.NewCoin(denom, math.NewInt(250_000)))

	_, _, sender := testdata.KeyTestPubAddr()
	_, _, recipient := testdata.KeyTestPubAddr()

	require.NoError(t, f.bankKeeper.MintCoins(f.sdkCtx, minttypes.ModuleName, mintAmt))
	require.NoError(t, f.bankKeeper.SendCoinsFromModuleToAccount(
		f.sdkCtx, minttypes.ModuleName, sender, mintAmt,
	))

	require.NoError(t, f.bankKeeper.SendCoins(f.sdkCtx, sender, recipient, sendAmt))

	require.Equal(t, "750000", f.bankKeeper.GetBalance(f.sdkCtx, sender, denom).Amount.String())
	require.Equal(t, "250000", f.bankKeeper.GetBalance(f.sdkCtx, recipient, denom).Amount.String())
}
