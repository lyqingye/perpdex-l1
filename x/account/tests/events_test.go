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

// TestPositionLifecycle_ApplyFill_OpenUpdateClose pins the three-event
// lifecycle contract (issue #91). Every transition is driven through
// the cohesive `ApplyFill` entry-point so the test exercises exactly
// the surface external callers (x/trade) use — no mut closures, no
// peeking at the package-private open/mutate/close primitives:
//
//   - Open  (BaseSize 0 → 5)  fires EventPositionOpened (new id),
//     no Updated.
//   - Same-side increase (5 → 8) fires EventPositionUpdated; the
//     position_id is preserved across the update.
//   - Pure close (8 → 0) fires EventPositionClosed (closing id
//     preserved on payload) and removes the row from storage.
func TestPositionLifecycle_ApplyFill_OpenUpdateClose(t *testing.T) {
	env := initTestEnv(t)

	// 1) Open: pre.BaseSize == 0, fill of +5 @ price 100 → BaseSize=5.
	resetEvents(env)
	opened, err := env.ak.ApplyFill(env.ctx, 7001, 0, 100, math.NewInt(5), math.ZeroInt())
	require.NoError(t, err)
	require.Equal(t, 1, countEvents(env, &types.EventPositionOpened{}),
		"ApplyFill on a fresh row must emit a single EventPositionOpened")
	require.Equal(t, 0, countEvents(env, &types.EventPositionUpdated{}),
		"ApplyFill on a fresh row must NOT emit EventPositionUpdated")
	require.NotZero(t, opened.New.PositionId,
		"ApplyFill must allocate a non-zero position_id on the open transition")
	require.True(t, opened.New.BaseSize.Equal(math.NewInt(5)))
	require.False(t, opened.SideFlipped)
	require.False(t, opened.Closed)

	// 2) Same-side increase: pre=5, fill of +3 → BaseSize=8.
	resetEvents(env)
	updated, err := env.ak.ApplyFill(env.ctx, 7001, 0, 100, math.NewInt(3), math.ZeroInt())
	require.NoError(t, err)
	require.Equal(t, 1, countEvents(env, &types.EventPositionUpdated{}),
		"ApplyFill same-side increase must emit a single EventPositionUpdated")
	require.Equal(t, 0, countEvents(env, &types.EventPositionOpened{}),
		"ApplyFill same-side increase must NOT emit EventPositionOpened")
	require.Equal(t, opened.New.PositionId, updated.New.PositionId,
		"position_id must be stable across same-side ApplyFill")
	require.True(t, updated.New.BaseSize.Equal(math.NewInt(8)))

	// 3) Pure close: pre=8, fill of -8 → BaseSize=0.
	resetEvents(env)
	closed, err := env.ak.ApplyFill(env.ctx, 7001, 0, 100, math.NewInt(-8), math.ZeroInt())
	require.NoError(t, err)
	require.Equal(t, 1, countEvents(env, &types.EventPositionClosed{}),
		"ApplyFill pure close must emit a single EventPositionClosed")
	require.True(t, closed.Closed, "FillApplyResult.Closed must be true on pure close")
	require.Equal(t, opened.New.PositionId, closed.New.PositionId,
		"ApplyFill close must surface the closing position_id on the pre-close snapshot")

	after, err := env.ak.GetPosition(env.ctx, 7001, 0)
	require.NoError(t, err)
	require.True(t, after.BaseSize.IsZero())
	require.Zero(t, after.PositionId)
}

// TestPositionLifecycle_ApplyFill_Flip pins the flip lifecycle: a
// reverse fill that crosses zero is handled INSIDE ApplyFill — the
// keeper emits Closed (old id) + Opened (new id) atomically. The
// indexer can finalise the previous lifeline before starting the
// next, without ever parsing fill math itself.
func TestPositionLifecycle_ApplyFill_Flip(t *testing.T) {
	env := initTestEnv(t)

	// Open a long of 10.
	opened, err := env.ak.ApplyFill(env.ctx, 7100, 1, 100, math.NewInt(10), math.ZeroInt())
	require.NoError(t, err)

	// Flip via a single ApplyFill: -15 against +10 → close +10, open -5.
	resetEvents(env)
	flipped, err := env.ak.ApplyFill(env.ctx, 7100, 1, 100, math.NewInt(-15), math.ZeroInt())
	require.NoError(t, err)
	require.True(t, flipped.SideFlipped, "ApplyFill must mark side flipped")
	require.False(t, flipped.Closed, "flip is not a Closed-only transition")
	require.Equal(t, 1, countEvents(env, &types.EventPositionClosed{}),
		"flip must emit EventPositionClosed for the old lifeline")
	require.Equal(t, 1, countEvents(env, &types.EventPositionOpened{}),
		"flip must emit EventPositionOpened for the new lifeline")
	require.NotEqual(t, opened.New.PositionId, flipped.New.PositionId,
		"flip must allocate a new position_id")
}

// TestPositionLifecycle_AdjustAllocatedMargin_Emits_Updated pins the
// AdjustAllocatedMargin contract: the cohesive isolated-margin RMW
// emits exactly one EventPositionUpdated per call and rejects calls
// against rows with BaseSize == 0 (no phantom balances).
func TestPositionLifecycle_AdjustAllocatedMargin_Emits_Updated(t *testing.T) {
	env := initTestEnv(t)

	_, err := env.ak.ApplyFill(env.ctx, 7400, 0, 100, math.NewInt(5), math.ZeroInt())
	require.NoError(t, err)

	resetEvents(env)
	updated, err := env.ak.AdjustAllocatedMargin(env.ctx, 7400, 0, math.NewInt(123))
	require.NoError(t, err)
	require.Equal(t, 1, countEvents(env, &types.EventPositionUpdated{}),
		"AdjustAllocatedMargin must emit a single EventPositionUpdated")
	require.True(t, updated.AllocatedMargin.Equal(math.NewInt(123)))

	// Zero delta short-circuits — no event.
	resetEvents(env)
	_, err = env.ak.AdjustAllocatedMargin(env.ctx, 7400, 0, math.ZeroInt())
	require.NoError(t, err)
	require.Equal(t, 0, countEvents(env, &types.EventPositionUpdated{}),
		"AdjustAllocatedMargin with zero delta must be a no-op")

	// AdjustAllocatedMargin against a fresh / empty row violates the
	// open-position precondition.
	_, err = env.ak.AdjustAllocatedMargin(env.ctx, 7401, 0, math.NewInt(50))
	require.ErrorIs(t, err, types.ErrPositionLifecycleViolation,
		"AdjustAllocatedMargin against an empty row must surface ErrPositionLifecycleViolation")
}

// TestPositionLifecycle_ApplyFundingPayment_Emits_Updated pins the
// cohesive ApplyFundingPayment contract: folds the per-position
// payment into EntryQuote and emits exactly one EventPositionUpdated;
// no-op (no event) on empty rows or zero-delta rounds.
func TestPositionLifecycle_ApplyFundingPayment_Emits_Updated(t *testing.T) {
	env := initTestEnv(t)

	// Open with current FundingRatePrefixSum == 0 (fakeMarket default).
	_, err := env.ak.ApplyFill(env.ctx, 7500, 0, 100, math.NewInt(10), math.ZeroInt())
	require.NoError(t, err)

	resetEvents(env)
	// Apply a prefix-sum jump → non-zero payment.
	tick := math.NewInt(perptypes.FundingRateTick)
	_, err = env.ak.ApplyFundingPayment(env.ctx, 7500, 0, tick)
	require.NoError(t, err)
	require.Equal(t, 1, countEvents(env, &types.EventPositionUpdated{}),
		"ApplyFundingPayment with a non-zero delta must emit EventPositionUpdated")

	// Re-apply with the same prefix sum → zero delta → no-op.
	resetEvents(env)
	_, err = env.ak.ApplyFundingPayment(env.ctx, 7500, 0, tick)
	require.NoError(t, err)
	require.Equal(t, 0, countEvents(env, &types.EventPositionUpdated{}),
		"ApplyFundingPayment with zero prefix delta must be a no-op")

	// Empty row → no-op, no event.
	resetEvents(env)
	_, err = env.ak.ApplyFundingPayment(env.ctx, 7501, 0, tick)
	require.NoError(t, err)
	require.Equal(t, 0, countEvents(env, &types.EventPositionUpdated{}),
		"ApplyFundingPayment on an empty row must be a no-op")
}

// TestPositionLifecycle_SetLeverage_Violations pins the
// SetPositionLeverage precondition (no open position) and the
// "no-default-noop" branch: writing the default leverage on a row
// that didn't previously exist is a no-op and does NOT emit an event.
func TestPositionLifecycle_SetLeverage_Violations(t *testing.T) {
	env := initTestEnv(t)

	// Default + no prior row → no-op, no event.
	resetEvents(env)
	require.NoError(t,
		env.ak.SetPositionLeverage(env.ctx, 7600, 0, perptypes.CrossMargin, 0))
	require.Equal(t, 0, countEvents(env, &types.EventPositionUpdated{}),
		"default leverage on an empty row must be a no-op")

	// Open a position, then SetPositionLeverage must fail loudly.
	_, err := env.ak.ApplyFill(env.ctx, 7600, 1, 100, math.NewInt(1), math.ZeroInt())
	require.NoError(t, err)
	err = env.ak.SetPositionLeverage(env.ctx, 7600, 1, perptypes.IsolatedMargin, 500)
	require.ErrorIs(t, err, types.ErrPositionLifecycleViolation,
		"SetPositionLeverage against an open position must violate the precondition")
}

// TestPositionLifecycle_ClosePosition_RejectsEmpty pins the force-
// close precondition: ClosePosition against an empty row fails
// loudly. The fill-driven close path is exercised via ApplyFill above
// (which short-circuits to closePosition internally).
func TestPositionLifecycle_ClosePosition_RejectsEmpty(t *testing.T) {
	env := initTestEnv(t)

	_, err := env.ak.ClosePosition(env.ctx, 9004, 0)
	require.ErrorIs(t, err, types.ErrPositionLifecycleViolation,
		"ClosePosition on an empty row must surface ErrPositionLifecycleViolation")
}

// TestPositionLifecycle_OutOfBounds pins the bounds-check on the
// cohesive ApplyFill: a post-trade |BaseSize| or |EntryQuote| that
// overflows the per-market wire encoding (POSITION_SIZE_BITS /
// ENTRY_QUOTE_BITS) surfaces as ErrPositionOutOfBounds, which the
// trade engine wraps into Maker/Taker InvalidPosition.
func TestPositionLifecycle_OutOfBounds(t *testing.T) {
	env := initTestEnv(t)

	// Drive BaseSize past MaxPositionSize: a single fill larger than
	// the bound is enough.
	overflowBase := perptypes.MaxPositionSize + 1
	_, err := env.ak.ApplyFill(env.ctx, 7700, 0, 1, math.NewIntFromUint64(overflowBase), math.ZeroInt())
	require.ErrorIs(t, err, types.ErrPositionOutOfBounds,
		"ApplyFill must reject out-of-bounds post-trade BaseSize")
}

// TestPositionId_UniqueAcrossLifecycles proves the position_id
// allocator is monotonic and never reuses an id across separate
// lifecycles (open → close → reopen yields a fresh id), so off-chain
// indexers can rely on it as a globally unique join key.
func TestPositionId_UniqueAcrossLifecycles(t *testing.T) {
	env := initTestEnv(t)

	open := func(acc uint64, mkt uint32) uint64 {
		r, err := env.ak.ApplyFill(env.ctx, acc, mkt, 100, math.NewInt(1), math.ZeroInt())
		require.NoError(t, err)
		return r.New.PositionId
	}
	closeFn := func(acc uint64, mkt uint32) {
		r, err := env.ak.ApplyFill(env.ctx, acc, mkt, 100, math.NewInt(-1), math.ZeroInt())
		require.NoError(t, err)
		require.True(t, r.Closed)
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
// "leverage-only retention" branch: closing a row that carries
// non-default leverage (via ApplyFill's close path) RETAINS the row
// (with base_size == 0 and position_id == 0) so the user's preferred
// leverage survives the close → reopen cycle. The event still fires
// with `deleted = false`.
func TestStateChangeEvents_Position_CloseRetainsLeverage(t *testing.T) {
	env := initTestEnv(t)

	// Seed the user's leverage preference, then open via ApplyFill.
	require.NoError(t, env.ak.SetPositionLeverage(env.ctx, 7300, 3, perptypes.IsolatedMargin, 500))
	_, err := env.ak.ApplyFill(env.ctx, 7300, 3, 100, math.NewInt(7), math.ZeroInt())
	require.NoError(t, err)

	resetEvents(env)
	_, err = env.ak.ApplyFill(env.ctx, 7300, 3, 100, math.NewInt(-7), math.ZeroInt())
	require.NoError(t, err)
	require.Equal(t, 1, countEvents(env, &types.EventPositionClosed{}),
		"ApplyFill close must emit EventPositionClosed even when row is retained")

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
