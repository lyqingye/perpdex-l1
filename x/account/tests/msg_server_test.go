// Msg server scenario tests for Deposit / Withdraw / Transfer /
// UpdateLeverage / UpdateMargin. The suite has two complementary
// halves:
//
//   - State-dependent scenario tests that seed accounts via the
//     keepertest helpers and assert that the handler enforces module
//     invariants (pool-account rejection, params-min floors, spot
//     locks, per-market IMF floor).
//   - Defense-in-depth tests that pin every handler to call
//     msg.ValidateBasic() on entry, so callers that bypass the SDK
//     ante (governance proposals, future cross-module routers, or
//     tests like these) cannot smuggle malformed messages past the
//     stateless invariants.
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

// TestWithdraw_RejectsPoolAccount pins the invariant that the generic
// Withdraw Msg refuses to touch PUBLIC_POOL / INSURANCE_FUND accounts.
func TestWithdraw_RejectsPoolAccount(t *testing.T) {
	env := initTestEnv(t)
	srv := accountkeeper.NewMsgServerImpl(env.ak)

	pool := types.Account{
		AccountIndex: 4242,
		OwnerAddress: validOwner,
		AccountType:  perptypes.PublicPoolAccountType,
		Collateral:   math.NewInt(1_000_000),
	}
	require.NoError(t, keepertest.SetAccountForTest(env.ctx, env.ak, pool))

	_, err := srv.Withdraw(env.ctx, &types.MsgWithdraw{
		Sender:       validOwner,
		AccountIndex: pool.AccountIndex,
		AssetIndex:   perptypes.USDCAssetIndex,
		Amount:       1000,
		RouteType:    perptypes.RouteTypePerps,
	})
	require.ErrorIs(t, err, types.ErrPoolGenericMsg)
}

// TestTransfer_RejectsMissingDestination short-circuits when the destination
// account does not exist, rather than silently creating a new row.
func TestTransfer_RejectsMissingDestination(t *testing.T) {
	env := initTestEnv(t)
	srv := accountkeeper.NewMsgServerImpl(env.ak)

	src := types.Account{
		AccountIndex: 501,
		OwnerAddress: validOwner,
		AccountType:  perptypes.MasterAccountType,
		Collateral:   math.NewInt(10_000),
	}
	require.NoError(t, keepertest.SetAccountForTest(env.ctx, env.ak, src))

	_, err := srv.Transfer(env.ctx, &types.MsgTransfer{
		Sender:           validOwner,
		FromAccountIndex: 501,
		ToAccountIndex:   999, // does not exist
		AssetIndex:       perptypes.USDCAssetIndex,
		Amount:           1,
	})
	require.ErrorIs(t, err, types.ErrAccountNotFound)
}

// TestUpdateLeverage_RejectsBelowMarketMinIMF makes sure UpdateLeverage
// honours the market-details floor (audit Medium account-7).
func TestUpdateLeverage_RejectsBelowMarketMinIMF(t *testing.T) {
	env := initTestEnv(t)
	env.market = fakeMarketKeeper{minImf: 500}
	env.ak.SetMarketKeeper(env.market)
	srv := accountkeeper.NewMsgServerImpl(env.ak)

	require.NoError(t, keepertest.SetAccountForTest(env.ctx, env.ak, types.Account{
		AccountIndex: 888,
		OwnerAddress: validOwner,
		AccountType:  perptypes.MasterAccountType,
		Collateral:   math.NewInt(1),
	}))

	_, err := srv.UpdateLeverage(env.ctx, &types.MsgUpdateLeverage{
		Sender:                   validOwner,
		AccountIndex:             888,
		MarketIndex:              0,
		NewInitialMarginFraction: 100, // below min 500
		NewMarginMode:            perptypes.CrossMargin,
	})
	require.ErrorIs(t, err, types.ErrInvalidParams)
}

// TestWithdraw_RespectsSpotLock validates that the spot Withdraw path
// cannot drain a balance below the resting LockedBalance reservation
// (audit H1: AddAccountAssetBalance must enforce Available on debit).
func TestWithdraw_RespectsSpotLock(t *testing.T) {
	env := initTestEnv(t)
	srv := accountkeeper.NewMsgServerImpl(env.ak)

	require.NoError(t, keepertest.SetAccountForTest(env.ctx, env.ak, types.Account{
		AccountIndex: 1001,
		OwnerAddress: validOwner,
		AccountType:  perptypes.MasterAccountType,
		Collateral:   math.ZeroInt(),
	}))
	// Set spot row with Balance large enough to satisfy minimum but
	// LockedBalance leaving only 5M USDC available, well under the
	// 10M minimum withdrawal. Without H1 the debit would succeed and
	// leave Balance < LockedBalance.
	require.NoError(t, keepertest.SetAccountAssetForTest(env.ctx, env.ak, types.AccountAsset{
		AccountIndex:  1001,
		AssetIndex:    perptypes.USDCAssetIndex,
		Balance:       internalAmount(15_000_000),
		LockedBalance: internalAmount(10_000_000),
		MarginMode:    perptypes.MarginModeEnabled,
	}))

	_, err := srv.Withdraw(env.ctx, &types.MsgWithdraw{
		Sender:       validOwner,
		AccountIndex: 1001,
		AssetIndex:   perptypes.USDCAssetIndex,
		Amount:       10_000_000,
		RouteType:    perptypes.RouteTypeSpot,
	})
	require.ErrorIs(t, err, types.ErrInsufficientFunds)
}

// TestWithdraw_RespectsParamsMin proves the module-level Params floor
// is now actually consulted on Withdraw (audit M1).
func TestWithdraw_RespectsParamsMin(t *testing.T) {
	env := initTestEnv(t)
	srv := accountkeeper.NewMsgServerImpl(env.ak)

	require.NoError(t, keepertest.SetAccountForTest(env.ctx, env.ak, types.Account{
		AccountIndex: 3001,
		OwnerAddress: validOwner,
		AccountType:  perptypes.MasterAccountType,
		Collateral:   internalAmount(1_000_000_000),
	}))
	// Inflate the module-level minimum well above the asset min so the
	// post-fix branch fires.
	require.NoError(t, env.ak.Params.Set(env.ctx, types.Params{
		MinPartialTransferAmount:      perptypes.MinPartialTransferAmount,
		MinPartialWithdrawAmount:      perptypes.MinPartialWithdrawAmount * 5,
		LiquidityPoolIndex:            perptypes.InsuranceFundOperatorAccountIdx,
		LiquidityPoolCooldownPeriodMs: perptypes.DefaultLLPCooldownPeriodMs,
	}))

	_, err := srv.Withdraw(env.ctx, &types.MsgWithdraw{
		Sender:       validOwner,
		AccountIndex: 3001,
		AssetIndex:   perptypes.USDCAssetIndex,
		Amount:       perptypes.MinPartialWithdrawAmount * 2, // above asset min but below new params min
		RouteType:    perptypes.RouteTypePerps,
	})
	require.ErrorIs(t, err, types.ErrAmountTooSmall)
}

// TestTransfer_RespectsParamsMin proves Transfer now enforces a
// minimum amount (audit M2 + M1).
func TestTransfer_RespectsParamsMin(t *testing.T) {
	env := initTestEnv(t)
	srv := accountkeeper.NewMsgServerImpl(env.ak)

	require.NoError(t, keepertest.SetAccountForTest(env.ctx, env.ak, types.Account{
		AccountIndex: 4001,
		OwnerAddress: validOwner,
		AccountType:  perptypes.MasterAccountType,
		Collateral:   internalAmount(1_000_000_000),
	}))
	require.NoError(t, keepertest.SetAccountForTest(env.ctx, env.ak, types.Account{
		AccountIndex: 4002,
		OwnerAddress: validOwner,
		AccountType:  perptypes.MasterAccountType,
		Collateral:   math.ZeroInt(),
	}))

	_, err := srv.Transfer(env.ctx, &types.MsgTransfer{
		Sender:           validOwner,
		FromAccountIndex: 4001,
		ToAccountIndex:   4002,
		AssetIndex:       perptypes.USDCAssetIndex,
		Amount:           1, // way below MinPartialTransferAmount
	})
	require.ErrorIs(t, err, types.ErrAmountTooSmall)
}

// The following tests pin the msg_server defense-in-depth contract:
// every public handler must call msg.ValidateBasic() on entry so that
// callers that bypass the SDK ante (keeper-level tests like the ones in
// this file, governance proposals routed through MsgServiceRouter,
// future cross-module Msg routers) cannot smuggle malformed messages
// past the stateless invariants. Each test constructs a message that
// passes the per-handler state-dependent checks (so any rejection MUST
// come from ValidateBasic) and asserts the handler returns the
// expected stateless error without touching state.

// TestMsgServer_Deposit_RejectsInvalidRoute proves the Deposit handler
// surfaces ValidateBasic's route-enum check even when called directly
// from the keeper layer.
func TestMsgServer_Deposit_RejectsInvalidRoute(t *testing.T) {
	env := initTestEnv(t)
	srv := accountkeeper.NewMsgServerImpl(env.ak)

	_, err := srv.Deposit(env.ctx, &types.MsgDeposit{
		Sender:     validOwner,
		AssetIndex: perptypes.USDCAssetIndex,
		Amount:     1_000_000,
		RouteType:  99, // out of {RouteTypePerps, RouteTypeSpot}
	})
	require.ErrorIs(t, err, types.ErrInvalidRoute)
}

// TestMsgServer_Withdraw_RejectsZeroAmount proves the Withdraw handler
// runs ValidateBasic before authorization / state lookups so a zero
// amount is rejected without touching the store.
func TestMsgServer_Withdraw_RejectsZeroAmount(t *testing.T) {
	env := initTestEnv(t)
	srv := accountkeeper.NewMsgServerImpl(env.ak)

	_, err := srv.Withdraw(env.ctx, &types.MsgWithdraw{
		Sender:       validOwner,
		AccountIndex: 1234,
		AssetIndex:   perptypes.USDCAssetIndex,
		Amount:       0,
		RouteType:    perptypes.RouteTypePerps,
	})
	require.ErrorIs(t, err, types.ErrAmountTooSmall)
}

// TestMsgServer_Transfer_RejectsSameAccount proves the Transfer
// handler rejects from == to via ValidateBasic before any pool /
// authorization checks run.
func TestMsgServer_Transfer_RejectsSameAccount(t *testing.T) {
	env := initTestEnv(t)
	srv := accountkeeper.NewMsgServerImpl(env.ak)

	_, err := srv.Transfer(env.ctx, &types.MsgTransfer{
		Sender:           validOwner,
		FromAccountIndex: 4242,
		ToAccountIndex:   4242,
		AssetIndex:       perptypes.USDCAssetIndex,
		Amount:           1_000,
	})
	require.ErrorIs(t, err, types.ErrInvalidParams)
}

// TestMsgServer_UpdateMargin_RejectsInvalidAction proves the
// UpdateMargin handler enforces the Action enum guard from
// ValidateBasic at the keeper-call layer.
func TestMsgServer_UpdateMargin_RejectsInvalidAction(t *testing.T) {
	env := initTestEnv(t)
	srv := accountkeeper.NewMsgServerImpl(env.ak)

	_, err := srv.UpdateMargin(env.ctx, &types.MsgUpdateMargin{
		Sender:       validOwner,
		AccountIndex: 9001,
		MarketIndex:  0,
		Action:       99, // not in {AddMargin, RemoveMargin}
		Amount:       math.NewInt(100),
	})
	require.ErrorIs(t, err, types.ErrInvalidMarginAction)
}

// TestMsgServer_UpdateLeverage_RejectsIMFAboveTick proves the
// UpdateLeverage handler enforces the MarginTick upper bound at the
// keeper-call layer (the per-market floor still requires
// MarketKeeper and is exercised by other tests).
func TestMsgServer_UpdateLeverage_RejectsIMFAboveTick(t *testing.T) {
	env := initTestEnv(t)
	srv := accountkeeper.NewMsgServerImpl(env.ak)

	_, err := srv.UpdateLeverage(env.ctx, &types.MsgUpdateLeverage{
		Sender:                   validOwner,
		AccountIndex:             9002,
		MarketIndex:              0,
		NewMarginMode:            perptypes.CrossMargin,
		NewInitialMarginFraction: uint32(perptypes.MarginTick) + 1,
	})
	require.ErrorIs(t, err, types.ErrInvalidParams)
}
