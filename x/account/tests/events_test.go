// State-change event emission contract. These tests pin the
// off-chain indexer contract that every cohesive Account /
// AccountAsset / AccountPosition mutator emits exactly one typed
// event per affected row. The event payload is what indexers stream
// to reconstruct the in-memory views; missing one means a silent
// state drift between chain and indexer.
package tests

import (
	"testing"

	"github.com/stretchr/testify/require"

	"cosmossdk.io/collections"
	"cosmossdk.io/math"

	sdk "github.com/cosmos/cosmos-sdk/types"

	perptypes "github.com/perpdex/perpdex-l1/types"
	"github.com/perpdex/perpdex-l1/x/account/keepertest"
	"github.com/perpdex/perpdex-l1/x/account/types"
)

// TestStateChangeEvents_AccountUpdate verifies that mutating an existing
// Account row through any cohesive mutator (here AddCollateral, which is
// the keeper API used by x/trade / x/funding / x/liquidation when they
// realise PnL or charge fees) fires an EventAccountUpdated{created=false}
// so off-chain indexers can reconstruct the Accounts table from the
// event stream — the whole reason these events were lifted out of the
// msg_server.
func TestStateChangeEvents_AccountUpdate(t *testing.T) {
	env := initTestEnv(t)

	const masterIdx uint64 = 7777
	require.NoError(t, keepertest.SetAccountForTest(env.ctx, env.ak, types.Account{
		AccountIndex: masterIdx,
		OwnerAddress: validOwner,
		AccountType:  perptypes.MasterAccountType,
		Collateral:   math.ZeroInt(),
	}))

	resetEvents(env)
	require.NoError(t, env.ak.AddCollateral(env.ctx, masterIdx, math.NewInt(123)))

	require.Equal(t, 1, countEvents(env, &types.EventAccountUpdated{}),
		"AddCollateral must emit a single EventAccountUpdated")
}

// TestStateChangeEvents_AccountCreate verifies that creating a sub
// account through the cohesive CreateSubAccount API fires an
// EventAccountUpdated{created=true} carrying the freshly-allocated row.
func TestStateChangeEvents_AccountCreate(t *testing.T) {
	env := initTestEnv(t)

	master, err := env.ak.EnsureMasterAccount(env.ctx, sdk.MustAccAddressFromBech32(validOwner))
	require.NoError(t, err)

	resetEvents(env)
	sub, err := env.ak.CreateSubAccount(env.ctx, master)
	require.NoError(t, err)

	require.Equal(t, 1, countEvents(env, &types.EventAccountUpdated{}),
		"CreateSubAccount must emit a single EventAccountUpdated for the new row")
	require.Equal(t, master.AccountIndex, sub.MasterAccountIndex)
}

// TestStateChangeEvents_AccountAsset_LockPath pins the invariant that
// the orderbook lock path (IncreaseLockedBalance, called by x/orderbook
// on every spot order placement) emits an EventAccountAssetUpdated, so
// indexers see every AccountAsset mutation triggered by resting orders.
func TestStateChangeEvents_AccountAsset_LockPath(t *testing.T) {
	env := initTestEnv(t)

	const accIdx uint64 = 6001
	const assetIdx uint32 = perptypes.USDCAssetIndex
	require.NoError(t, env.ak.AccountAssets.Set(env.ctx,
		collections.Join(accIdx, assetIdx),
		types.AccountAsset{
			AccountIndex:  accIdx,
			AssetIndex:    assetIdx,
			Balance:       math.NewInt(1_000),
			LockedBalance: math.ZeroInt(),
		}))

	resetEvents(env)
	require.NoError(t, env.ak.IncreaseLockedBalance(env.ctx, accIdx, assetIdx, math.NewInt(400)))
	require.Equal(t, 1, countEvents(env, &types.EventAccountAssetUpdated{}),
		"IncreaseLockedBalance must emit EventAccountAssetUpdated (orderbook path)")

	resetEvents(env)
	require.NoError(t, env.ak.DecreaseLockedBalance(env.ctx, accIdx, assetIdx, math.NewInt(100)))
	require.Equal(t, 1, countEvents(env, &types.EventAccountAssetUpdated{}),
		"DecreaseLockedBalance must emit EventAccountAssetUpdated (orderbook path)")
}

// TestStateChangeEvents_AccountAsset_TransferPath proves the x/trade
// spot-fill path also surfaces events (one per side of the transfer).
func TestStateChangeEvents_AccountAsset_TransferPath(t *testing.T) {
	env := initTestEnv(t)

	const fromIdx uint64 = 6101
	const toIdx uint64 = 6102
	const assetIdx uint32 = perptypes.USDCAssetIndex
	require.NoError(t, env.ak.AccountAssets.Set(env.ctx,
		collections.Join(fromIdx, assetIdx),
		types.AccountAsset{
			AccountIndex: fromIdx, AssetIndex: assetIdx,
			Balance: math.NewInt(1_000), LockedBalance: math.ZeroInt(),
		}))
	require.NoError(t, env.ak.AccountAssets.Set(env.ctx,
		collections.Join(toIdx, assetIdx),
		types.AccountAsset{
			AccountIndex: toIdx, AssetIndex: assetIdx,
			Balance: math.ZeroInt(), LockedBalance: math.ZeroInt(),
		}))

	resetEvents(env)
	require.NoError(t, env.ak.TransferAccountAssetBalance(
		env.ctx, fromIdx, toIdx, assetIdx, math.NewInt(200), false /* drainLockedFirst */))
	require.Equal(t, 2, countEvents(env, &types.EventAccountAssetUpdated{}),
		"TransferAccountAssetBalance must emit one EventAccountAssetUpdated per side")
}

// TestStateChangeEvents_Position proves that the x/trade / x/funding
// position-update path (UpdatePosition) fires a position event. This
// is the most critical of the three because positions are mutated
// almost exclusively from outside x/account.
func TestStateChangeEvents_Position(t *testing.T) {
	env := initTestEnv(t)

	resetEvents(env)
	_, err := env.ak.UpdatePosition(env.ctx, 7001, 0, func(p *types.AccountPosition) error {
		p.BaseSize = math.NewInt(5)
		p.EntryQuote = math.NewInt(500)
		p.AllocatedMargin = math.NewInt(50)
		return nil
	})
	require.NoError(t, err)
	require.Equal(t, 1, countEvents(env, &types.EventPositionUpdated{}),
		"UpdatePosition must emit a single EventPositionUpdated")
}
