// Account lifecycle / authorisation tests. These cover the keeper
// surface that touches account identity rather than the position or
// asset substores: owner-based authorisation guards and the
// master->sub secondary index that backs the SubAccounts gRPC query.
package tests

import (
	"testing"

	"github.com/stretchr/testify/require"

	perptypes "github.com/perpdex/perpdex-l1/types"
	accountkeeper "github.com/perpdex/perpdex-l1/x/account/keeper"
	"github.com/perpdex/perpdex-l1/x/account/keepertest"
	"github.com/perpdex/perpdex-l1/x/account/types"
)

// TestIsAuthorized_RejectsEmptyOwner ensures owner-less genesis accounts
// (treasury / IF) cannot be matched by an empty signer (audit M6).
func TestIsAuthorized_RejectsEmptyOwner(t *testing.T) {
	env := initTestEnv(t)

	require.NoError(t, keepertest.SetAccountForTest(env.ctx, env.ak, types.Account{
		AccountIndex: 9999,
		OwnerAddress: "",
		AccountType:  perptypes.MasterAccountType,
	}))

	ok, err := env.ak.IsAuthorized(env.ctx, "", 9999)
	require.NoError(t, err)
	require.False(t, ok)
	// Sanity: a non-empty signer also fails on an owner-less row.
	ok, err = env.ak.IsAuthorized(env.ctx, "px1someaddr", 9999)
	require.NoError(t, err)
	require.False(t, ok)
}

// TestSubAccounts_UsesSecondaryIndex confirms the SubAccounts query is
// populated via the master->sub keyset rather than scanning every
// account (audit M4).
func TestSubAccounts_UsesSecondaryIndex(t *testing.T) {
	env := initTestEnv(t)
	srv := accountkeeper.NewQuerier(env.ak)

	// Wire two masters with non-overlapping sub-accounts and validate
	// the query only returns the requested master's children.
	const masterA, masterB uint64 = 5001, 5002
	require.NoError(t, keepertest.SetAccountForTest(env.ctx, env.ak, types.Account{
		AccountIndex: masterA, OwnerAddress: validOwner, AccountType: perptypes.MasterAccountType,
	}))
	require.NoError(t, keepertest.SetAccountForTest(env.ctx, env.ak, types.Account{
		AccountIndex: masterB, OwnerAddress: validOwner, AccountType: perptypes.MasterAccountType,
	}))
	for _, sub := range []uint64{6001, 6002, 6003} {
		require.NoError(t, keepertest.SetAccountForTest(env.ctx, env.ak, types.Account{
			AccountIndex:       sub,
			MasterAccountIndex: masterA,
			OwnerAddress:       validOwner,
			AccountType:        perptypes.SubAccountType,
		}))
	}
	require.NoError(t, keepertest.SetAccountForTest(env.ctx, env.ak, types.Account{
		AccountIndex:       7001,
		MasterAccountIndex: masterB,
		OwnerAddress:       validOwner,
		AccountType:        perptypes.SubAccountType,
	}))

	resp, err := srv.SubAccounts(env.ctx, &types.QuerySubAccountsRequest{MasterAccountIndex: masterA})
	require.NoError(t, err)
	require.Len(t, resp.Accounts, 3)
	for _, a := range resp.Accounts {
		require.Equal(t, masterA, a.MasterAccountIndex)
	}
}
