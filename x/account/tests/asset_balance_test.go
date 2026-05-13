// AccountAsset balance keeper invariants. Covers the low-level
// AddAccountAssetBalance hook that protects resting spot order locks
// from being raided by Withdraw / Transfer / TransferAccountAssetBalance
// (audit H1).
package tests

import (
	"testing"

	"github.com/stretchr/testify/require"

	perptypes "github.com/perpdex/perpdex-l1/types"
	"github.com/perpdex/perpdex-l1/x/account/keepertest"
	"github.com/perpdex/perpdex-l1/x/account/types"
)

// TestAddAccountAssetBalance_RespectsLock isolates H1: a negative
// delta cannot drain Balance below the resting LockedBalance, even
// though the post-state Balance would still be non-negative. This is
// the invariant that protects spot Withdraw / Transfer from raiding
// resting spot order locks.
func TestAddAccountAssetBalance_RespectsLock(t *testing.T) {
	env := initTestEnv(t)

	require.NoError(t, keepertest.SetAccountAssetForTest(env.ctx, env.ak, types.AccountAsset{
		AccountIndex:  2001,
		AssetIndex:    perptypes.USDCAssetIndex,
		Balance:       internalAmount(20),
		LockedBalance: internalAmount(15),
		MarginMode:    perptypes.MarginModeDisabled,
	}))
	require.ErrorIs(t,
		env.ak.AddAccountAssetBalance(env.ctx, 2001, perptypes.USDCAssetIndex, internalAmount(10).Neg()),
		types.ErrInsufficientFunds,
	)
	// Withdrawing within Available succeeds.
	require.NoError(t,
		env.ak.AddAccountAssetBalance(env.ctx, 2001, perptypes.USDCAssetIndex, internalAmount(5).Neg()),
	)
}
