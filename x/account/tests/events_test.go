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

// TestStateChangeEvents_Position_Lifecycle pins the three-event
// lifecycle contract (issue #91): OpenPosition (0 -> !=0) emits
// EventPositionOpened with a freshly allocated position_id; a
// subsequent MutatePosition emits EventPositionUpdated preserving the
// id; ClosePosition emits EventPositionClosed and removes the row
// from storage. The three events are the canonical lifeline an
// off-chain indexer streams to rebuild the per-position record.
func TestStateChangeEvents_Position_Lifecycle(t *testing.T) {
	env := initTestEnv(t)

	// 1) OpenPosition emits EventPositionOpened with a new id.
	resetEvents(env)
	opened, err := env.ak.OpenPosition(env.ctx, 7001, 0, func(p *types.AccountPosition) error {
		p.BaseSize = math.NewInt(5)
		p.EntryQuote = math.NewInt(500)
		p.AllocatedMargin = math.NewInt(50)
		return nil
	})
	require.NoError(t, err)
	require.Equal(t, 1, countEvents(env, &types.EventPositionOpened{}),
		"OpenPosition must emit a single EventPositionOpened")
	require.Equal(t, 0, countEvents(env, &types.EventPositionUpdated{}),
		"OpenPosition must NOT emit EventPositionUpdated")
	require.NotZero(t, opened.PositionId, "OpenPosition must allocate a non-zero position_id")

	// 2) MutatePosition: same-side update preserves the position_id.
	resetEvents(env)
	updated, err := env.ak.MutatePosition(env.ctx, 7001, 0, func(p *types.AccountPosition) error {
		p.BaseSize = math.NewInt(8)
		return nil
	})
	require.NoError(t, err)
	require.Equal(t, 1, countEvents(env, &types.EventPositionUpdated{}),
		"MutatePosition must emit a single EventPositionUpdated")
	require.Equal(t, 0, countEvents(env, &types.EventPositionOpened{}),
		"MutatePosition must NOT emit EventPositionOpened")
	require.Equal(t, opened.PositionId, updated.PositionId,
		"position_id must be stable across MutatePosition calls")

	// 3) ClosePosition removes the row (default leverage => not
	// retained) and emits EventPositionClosed carrying the closing
	// position_id.
	resetEvents(env)
	closed, err := env.ak.ClosePosition(env.ctx, 7001, 0)
	require.NoError(t, err)
	require.Equal(t, 1, countEvents(env, &types.EventPositionClosed{}),
		"ClosePosition must emit a single EventPositionClosed")
	require.Equal(t, opened.PositionId, closed.PositionId,
		"ClosePosition must surface the closing position_id on the returned snapshot")

	// Row is gone from storage; subsequent GetPosition auto-vivifies a
	// fresh zero record with position_id == 0.
	after, err := env.ak.GetPosition(env.ctx, 7001, 0)
	require.NoError(t, err)
	require.True(t, after.BaseSize.IsZero())
	require.Zero(t, after.PositionId)
}

// TestStateChangeEvents_Position_Flip pins the flip lifecycle: a
// caller-orchestrated ClosePosition + OpenPosition emits Closed
// (old id) + Opened (new id) so the indexer can finalise the previous
// lifeline before starting the next one. This is the contract x/trade
// `applyPositionChange` relies on when it detects `fill.SideFlipped`.
func TestStateChangeEvents_Position_Flip(t *testing.T) {
	env := initTestEnv(t)

	// Open a long position.
	opened, err := env.ak.OpenPosition(env.ctx, 7100, 1, func(p *types.AccountPosition) error {
		p.BaseSize = math.NewInt(10)
		p.EntryQuote = math.NewInt(1000)
		return nil
	})
	require.NoError(t, err)

	// Flip = explicit Close then Open.
	resetEvents(env)
	_, err = env.ak.ClosePosition(env.ctx, 7100, 1)
	require.NoError(t, err)
	flipped, err := env.ak.OpenPosition(env.ctx, 7100, 1, func(p *types.AccountPosition) error {
		p.BaseSize = math.NewInt(-5)
		p.EntryQuote = math.NewInt(-500)
		return nil
	})
	require.NoError(t, err)
	require.Equal(t, 1, countEvents(env, &types.EventPositionClosed{}),
		"flip must emit EventPositionClosed for the old lifeline")
	require.Equal(t, 1, countEvents(env, &types.EventPositionOpened{}),
		"flip must emit EventPositionOpened for the new lifeline")
	require.NotEqual(t, opened.PositionId, flipped.PositionId,
		"flip must allocate a new position_id")
}

// TestPositionLifecycle_Violations pins the negative-path assertions:
// OpenPosition rejects an already-open row, MutatePosition rejects
// empty / zeroing / sign-flipping mutations, and ClosePosition rejects
// a non-existent position. These guards are what make the
// three-method API "explicit" — a buggy caller fails loudly instead
// of silently round-tripping through a generic dispatcher.
func TestPositionLifecycle_Violations(t *testing.T) {
	env := initTestEnv(t)

	// OpenPosition rejects an already-open row.
	_, err := env.ak.OpenPosition(env.ctx, 9001, 0, func(p *types.AccountPosition) error {
		p.BaseSize = math.NewInt(5)
		p.EntryQuote = math.NewInt(500)
		return nil
	})
	require.NoError(t, err)
	_, err = env.ak.OpenPosition(env.ctx, 9001, 0, func(p *types.AccountPosition) error {
		p.BaseSize = math.NewInt(5)
		return nil
	})
	require.ErrorIs(t, err, types.ErrPositionLifecycleViolation,
		"OpenPosition on an already-open row must surface ErrPositionLifecycleViolation")

	// OpenPosition rejects a mutator that leaves BaseSize == 0.
	_, err = env.ak.OpenPosition(env.ctx, 9002, 0, func(p *types.AccountPosition) error {
		return nil // BaseSize stays at 0.
	})
	require.ErrorIs(t, err, types.ErrPositionLifecycleViolation,
		"OpenPosition mutator must leave BaseSize != 0")

	// MutatePosition rejects an empty row (use OpenPosition).
	_, err = env.ak.MutatePosition(env.ctx, 9003, 0, func(p *types.AccountPosition) error {
		p.BaseSize = math.NewInt(1)
		return nil
	})
	require.ErrorIs(t, err, types.ErrPositionLifecycleViolation,
		"MutatePosition on an empty row must surface ErrPositionLifecycleViolation")

	// MutatePosition rejects a mutator that zeros BaseSize (use
	// ClosePosition) or flips the sign (use Close + Open).
	_, err = env.ak.MutatePosition(env.ctx, 9001, 0, func(p *types.AccountPosition) error {
		p.BaseSize = math.NewInt(0)
		return nil
	})
	require.ErrorIs(t, err, types.ErrPositionLifecycleViolation,
		"MutatePosition zeroing BaseSize must surface ErrPositionLifecycleViolation")
	_, err = env.ak.MutatePosition(env.ctx, 9001, 0, func(p *types.AccountPosition) error {
		p.BaseSize = math.NewInt(-3)
		return nil
	})
	require.ErrorIs(t, err, types.ErrPositionLifecycleViolation,
		"MutatePosition flipping the sign must surface ErrPositionLifecycleViolation")

	// ClosePosition rejects an empty row.
	_, err = env.ak.ClosePosition(env.ctx, 9004, 0)
	require.ErrorIs(t, err, types.ErrPositionLifecycleViolation,
		"ClosePosition on an empty row must surface ErrPositionLifecycleViolation")
}

// TestPositionId_UniqueAcrossLifecycles proves the position_id
// allocator is monotonic and never reuses an id across separate
// lifecycles (open → close → reopen yields a fresh id), so off-chain
// indexers can rely on it as a globally unique join key.
func TestPositionId_UniqueAcrossLifecycles(t *testing.T) {
	env := initTestEnv(t)

	open := func(acc uint64, mkt uint32) uint64 {
		p, err := env.ak.OpenPosition(env.ctx, acc, mkt, func(p *types.AccountPosition) error {
			p.BaseSize = math.NewInt(1)
			p.EntryQuote = math.NewInt(100)
			return nil
		})
		require.NoError(t, err)
		return p.PositionId
	}
	closeFn := func(acc uint64, mkt uint32) {
		_, err := env.ak.ClosePosition(env.ctx, acc, mkt)
		require.NoError(t, err)
	}

	id1 := open(8001, 0)
	closeFn(8001, 0)
	id2 := open(8001, 0) // same (account, market) — fresh id
	closeFn(8001, 0)
	id3 := open(8002, 0) // different account — fresh id
	id4 := open(8001, 1) // different market — fresh id

	ids := []uint64{id1, id2, id3, id4}
	for i, id := range ids {
		require.NotZero(t, id, "ids[%d] must be non-zero", i)
		for j := 0; j < i; j++ {
			require.NotEqual(t, ids[j], id, "ids[%d] (%d) and ids[%d] (%d) must differ", i, id, j, ids[j])
		}
	}
}

// TestStateChangeEvents_Position_LeverageOnly proves the leverage-only
// configuration path: SetPositionLeverage against an empty position
// emits EventPositionUpdated (configuration write, not a lifecycle
// transition) and the persisted row carries position_id == 0 so
// indexers can distinguish it from a real open lifeline.
func TestStateChangeEvents_Position_LeverageOnly(t *testing.T) {
	env := initTestEnv(t)

	resetEvents(env)
	require.NoError(t, env.ak.SetPositionLeverage(env.ctx, 7200, 2, perptypes.IsolatedMargin, 500))
	require.Equal(t, 1, countEvents(env, &types.EventPositionUpdated{}),
		"SetPositionLeverage must emit EventPositionUpdated")
	require.Equal(t, 0, countEvents(env, &types.EventPositionOpened{}),
		"SetPositionLeverage must NOT emit EventPositionOpened")

	p, err := env.ak.GetPosition(env.ctx, 7200, 2)
	require.NoError(t, err)
	require.Zero(t, p.PositionId, "leverage-only row must keep position_id == 0")
	require.Equal(t, uint32(perptypes.IsolatedMargin), p.MarginMode)
	require.Equal(t, uint32(500), p.InitialMarginFraction)
}

// TestStateChangeEvents_Position_CloseRetainsLeverage proves the
// "leverage-only retention" branch: ClosePosition on a row that
// carries non-default leverage RETAINS the row (with base_size == 0
// and position_id == 0) so the user's preferred leverage survives the
// close → reopen cycle. The event still fires with `deleted = false`.
func TestStateChangeEvents_Position_CloseRetainsLeverage(t *testing.T) {
	env := initTestEnv(t)

	// Seed the user's leverage preference, then open a position. The
	// open inherits the leverage from the existing leverage-only row.
	require.NoError(t, env.ak.SetPositionLeverage(env.ctx, 7300, 3, perptypes.IsolatedMargin, 500))
	_, err := env.ak.OpenPosition(env.ctx, 7300, 3, func(p *types.AccountPosition) error {
		p.BaseSize = math.NewInt(7)
		p.EntryQuote = math.NewInt(700)
		return nil
	})
	require.NoError(t, err)

	resetEvents(env)
	_, err = env.ak.ClosePosition(env.ctx, 7300, 3)
	require.NoError(t, err)
	require.Equal(t, 1, countEvents(env, &types.EventPositionClosed{}),
		"ClosePosition must emit EventPositionClosed even when row is retained")

	// Row must STILL exist (with leverage intact) because the user
	// configured a non-default margin mode.
	p, err := env.ak.GetPosition(env.ctx, 7300, 3)
	require.NoError(t, err)
	require.True(t, p.BaseSize.IsZero(), "retained row must have base_size == 0")
	require.Zero(t, p.PositionId, "retained row must reset position_id to 0")
	require.Equal(t, uint32(perptypes.IsolatedMargin), p.MarginMode,
		"leverage must survive the close")
	require.Equal(t, uint32(500), p.InitialMarginFraction)
}
