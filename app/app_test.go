package app_test

import (
	"testing"

	"github.com/stretchr/testify/require"

	tmproto "github.com/cometbft/cometbft/proto/tendermint/types"

	authtypes "github.com/cosmos/cosmos-sdk/x/auth/types"
	stakingtypes "github.com/cosmos/cosmos-sdk/x/staking/types"

	perpapp "github.com/perpdex/perpdex-l1/app"
	"github.com/perpdex/perpdex-l1/app/helpers"
	perptypes "github.com/perpdex/perpdex-l1/types"
)

// TestPerpDEXApp_Setup boots a full PerpDEXApp via the test helpers and
// asserts that the app name is what we expect.
func TestPerpDEXApp_Setup(t *testing.T) {
	app := helpers.Setup(t)
	require.Equal(t, perpapp.AppName, app.Name())
}

// TestPerpDEXApp_GenesisInvariants exercises the helper constructor and
// checks the staking + bank module state ended up with the expected denom.
func TestPerpDEXApp_GenesisInvariants(t *testing.T) {
	app := helpers.Setup(t)
	ctx := app.NewUncachedContext(true, tmproto.Header{Height: app.LastBlockHeight()})

	stakingParams, err := app.StakingKeeper.GetParams(ctx)
	require.NoError(t, err)
	require.Equal(t, perptypes.UPerpDenom, stakingParams.BondDenom)

	bondedPool := authtypes.NewModuleAddress(stakingtypes.BondedPoolName)
	bondedBal := app.BankKeeper.GetBalance(ctx, bondedPool, perptypes.UPerpDenom)
	require.True(t, bondedBal.Amount.IsPositive(), "bonded pool must hold uperp")
}
