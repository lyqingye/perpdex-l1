// PublicPool keeper invariants. Covers the operator-share floor
// helper that gates withdrawals out of a pool (CheckMinOperatorShareRate)
// and the post-state risk-regression guard that the master must pass
// when seeding or topping up a public pool (audit H2).
package tests

import (
	"testing"

	"github.com/stretchr/testify/require"

	"cosmossdk.io/math"

	perptypes "github.com/perpdex/perpdex-l1/types"
	accountkeeper "github.com/perpdex/perpdex-l1/x/account/keeper"
	"github.com/perpdex/perpdex-l1/x/account/keepertest"
	"github.com/perpdex/perpdex-l1/x/account/types"
)

// TestCheckMinOperatorShareRate_EmptyPool short-circuits the skin-in-game
// check for pools that have not yet minted shares.
func TestCheckMinOperatorShareRate_EmptyPool(t *testing.T) {
	info := types.PublicPoolInfo{
		TotalShares:          math.ZeroInt(),
		OperatorShares:       math.ZeroInt(),
		MinOperatorShareRate: 5000,
	}
	require.True(t, accountkeeper.CheckMinOperatorShareRate(info))
}

// TestCheckMinOperatorShareRate_Violated rejects an operator whose share
// balance dipped below the configured floor.
func TestCheckMinOperatorShareRate_Violated(t *testing.T) {
	info := types.PublicPoolInfo{
		TotalShares:          math.NewInt(1000),
		OperatorShares:       math.NewInt(10),
		MinOperatorShareRate: 5000, // 50% floor
	}
	require.False(t, accountkeeper.CheckMinOperatorShareRate(info))
}

// TestMintShares_BlockedByRiskRegression verifies the master's
// post-state risk is enforced on MintShares (audit H2).
func TestMintShares_BlockedByRiskRegression(t *testing.T) {
	env := initTestEnv(t)
	srv := accountkeeper.NewMsgServerImpl(env.ak)

	const masterIdx uint64 = 8001
	require.NoError(t, keepertest.SetAccountForTest(env.ctx, env.ak, types.Account{
		AccountIndex: masterIdx,
		OwnerAddress: validOwner,
		AccountType:  perptypes.MasterAccountType,
		Collateral:   internalAmount(1_000_000_000),
	}))
	// Seed a public pool with non-zero total shares so USDCValueToShares
	// uses the NAV branch (which calls riskKeeper.GetTotalAccountValue).
	require.NoError(t, keepertest.SetAccountForTest(env.ctx, env.ak, types.Account{
		AccountIndex:       9001,
		MasterAccountIndex: masterIdx,
		OwnerAddress:       validOwner,
		AccountType:        perptypes.PublicPoolAccountType,
		Collateral:         internalAmount(1_000),
		PublicPoolInfo: &types.PublicPoolInfo{
			Status:               perptypes.PublicPoolStatusActive,
			OperatorFee:          0,
			MinOperatorShareRate: 0,
			TotalShares:          math.ZeroInt(),
			OperatorShares:       math.ZeroInt(),
			Strategies:           make([]math.Int, perptypes.NbStrategies),
		},
	}))
	// Mark fakeRiskKeeper as risky so the post-mint risk check rejects.
	env.risk.risky = true

	_, err := srv.MintShares(env.ctx, &types.MsgMintShares{
		Sender:           validOwner,
		PoolAccountIndex: 9001,
		PrincipalAmount:  1_000_000,
	})
	require.ErrorIs(t, err, types.ErrRiskRegression)
}

// TestCreatePublicPool_BlockedByRiskRegression mirrors the above for
// the pool-creation seed transfer (audit H2).
func TestCreatePublicPool_BlockedByRiskRegression(t *testing.T) {
	env := initTestEnv(t)
	srv := accountkeeper.NewMsgServerImpl(env.ak)

	const masterIdx uint64 = 8101
	// Master pays initial_total_shares * INITIAL_POOL_SHARE_VALUE *
	// USDC_TO_COLLATERAL = 10 * 1000 * 1_000_000 = 10_000_000_000.
	require.NoError(t, keepertest.SetAccountForTest(env.ctx, env.ak, types.Account{
		AccountIndex: masterIdx,
		OwnerAddress: validOwner,
		AccountType:  perptypes.MasterAccountType,
		Collateral:   internalAmount(1_000_000_000),
	}))
	env.risk.risky = true

	_, err := srv.CreatePublicPool(env.ctx, &types.MsgCreatePublicPool{
		Sender:               validOwner,
		MasterAccountIndex:   masterIdx,
		AccountType:          perptypes.PublicPoolAccountType,
		OperatorFee:          0,
		MinOperatorShareRate: 0,
		InitialTotalShares:   10,
	})
	require.ErrorIs(t, err, types.ErrRiskRegression)
}
