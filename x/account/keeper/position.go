package keeper

import (
	"context"
	"errors"

	"cosmossdk.io/collections"
	"cosmossdk.io/math"

	sdk "github.com/cosmos/cosmos-sdk/types"

	perptypes "github.com/perpdex/perpdex-l1/types"
	"github.com/perpdex/perpdex-l1/x/account/types"
)

// Position keeper surface (issue #91). x/account owns every
// AccountPosition write; external callers (x/trade, x/funding, the
// x/account msg_server) ONLY talk to it through cohesive,
// single-purpose methods — never a generic RMW closure.
//
// Cohesive public API:
//
//   ApplyFill(acc, mkt, price, baseAmount, sign) → FillApplyResult
//       The canonical entry-point for the trade engine. Computes the
//       fill math (AccountPosition.ApplyFill), classifies the
//       transition (open / mutate / close / flip), persists through
//       the matching package-private primitive, emits exactly one
//       (or, for flip, two) lifecycle events, and returns the
//       pre/post snapshots + realized PnL + OI delta the engine needs.
//
//   AdjustAllocatedMargin(acc, mkt, delta) → AccountPosition
//       Cohesive RMW for the isolated-margin pool. Used by the trade
//       engine's three-step isolated reconciliation (PnL/fee credit,
//       improvement-fee debit, position_requirement rebalance) and by
//       the msg_server UpdateMargin path. Asserts the row is open
//       (BaseSize != 0); emits EventPositionUpdated.
//
//   ApplyFundingPayment(acc, mkt, newPrefixSum) → AccountPosition
//       Cohesive RMW for funding settlement. Folds the per-position
//       payment into EntryQuote and snapshots the prefix sum.
//       No-op on empty rows (closed positions have no funding
//       obligation; OpenPosition re-seeds the snapshot from the
//       market's current value).
//
//   SetPositionLeverage(acc, mkt, mode, imf)
//       Writes a leverage-only configuration row (BaseSize == 0,
//       position_id == 0). Asserts no open position. Emits
//       EventPositionUpdated so indexers see the config change.
//
//   ClosePosition(acc, mkt) → AccountPosition
//       Cohesive force-close (e.g. for liquidation paths that bypass
//       ApplyFill, market expiry, IF/ADL absorbers). Used internally
//       by ApplyFill and re-exported for callers that need to retire
//       a position outside the fill pipeline.
//
// Package-private primitives (open / mutate / close, all lowercase)
// implement the three lifecycle transitions but are NOT part of the
// public surface — they exist purely so the cohesive methods above
// can share the persistence + event-emission plumbing.

// setPosition is the package-private write primitive used by genesis
// restore and by the lifecycle primitives below. It does NOT emit any
// event — emission is the lifecycle primitive's job.
func (k Keeper) setPosition(ctx context.Context, p types.AccountPosition) error {
	return k.AccountPositions.Set(ctx, collections.Join(p.AccountIndex, p.MarketIndex), p)
}

// removePosition is the package-private delete primitive. Used by
// closePosition's "default-leverage" branch to release KV storage.
func (k Keeper) removePosition(ctx context.Context, accIdx uint64, marketIdx uint32) error {
	return k.AccountPositions.Remove(ctx, collections.Join(accIdx, marketIdx))
}

func (k Keeper) emitPositionOpened(ctx context.Context, p types.AccountPosition) error {
	return sdk.UnwrapSDKContext(ctx).EventManager().EmitTypedEvent(&types.EventPositionOpened{
		Position: p,
	})
}

func (k Keeper) emitPositionUpdated(ctx context.Context, p types.AccountPosition) error {
	return sdk.UnwrapSDKContext(ctx).EventManager().EmitTypedEvent(&types.EventPositionUpdated{
		Position: p,
	})
}

func (k Keeper) emitPositionClosed(ctx context.Context, p types.AccountPosition, deleted bool) error {
	return sdk.UnwrapSDKContext(ctx).EventManager().EmitTypedEvent(&types.EventPositionClosed{
		Position: p,
		Deleted:  deleted,
	})
}

// hasNonDefaultLeverage reports whether the row carries user-configured
// leverage state that must survive a close → reopen cycle. The
// "default" baseline matches GetPosition's auto-vivified zero record:
// Cross margin and IMF == 0 (i.e. fall back to the market default).
func hasNonDefaultLeverage(p types.AccountPosition) bool {
	return p.MarginMode != perptypes.CrossMargin || p.InitialMarginFraction != 0
}

// withinPositionBounds enforces |position| < 2^POSITION_SIZE_BITS and
// |entry_quote| < 2^ENTRY_QUOTE_BITS. Used by ApplyFill to reject a
// post-trade state that overflows the per-market wire encoding.
func withinPositionBounds(position, entryQuote math.Int) bool {
	maxPos := math.NewIntFromUint64(perptypes.MaxPositionSize)
	maxEntryQuote := math.NewIntFromUint64(perptypes.MaxEntryQuote)
	if position.Abs().GT(maxPos) {
		return false
	}
	if entryQuote.Abs().GT(maxEntryQuote) {
		return false
	}
	return true
}

// GetPosition returns the position; an empty zero-valued one if absent.
// An empty zero-valued response carries `BaseSize == 0` and is used by
// callers as the "no open position" sentinel; the auto-vivified record
// is NOT persisted unless the caller subsequently routes through a
// cohesive write method.
func (k Keeper) GetPosition(ctx context.Context, accIdx uint64, marketIdx uint32) (types.AccountPosition, error) {
	p, err := k.AccountPositions.Get(ctx, collections.Join(accIdx, marketIdx))
	if err != nil {
		if errors.Is(err, collections.ErrNotFound) {
			return types.AccountPosition{
				AccountIndex:             accIdx,
				MarketIndex:              marketIdx,
				BaseSize:                 math.ZeroInt(),
				EntryQuote:               math.ZeroInt(),
				LastFundingRatePrefixSum: math.ZeroInt(),
				AllocatedMargin:          math.ZeroInt(),
				MarginMode:               perptypes.CrossMargin,
			}, nil
		}
		return types.AccountPosition{}, err
	}
	p.NormalizeIntFields()
	return p, nil
}

// IterateAccountPositions walks every persisted AccountPosition row
// owned by `accountIdx`. The callback returns `true` to stop early.
//
// Per-account driver for risk / liquidation / funding loops
// (ComputeCrossRisk, IsValidRiskChangeFrom, SnapshotRisk,
// IterateIsolatedPositions, processAccount, rankVictimPositionsByUPnL,
// settleAllPositionFunding) so they touch only persisted rows instead
// of scanning the full MaxPerpsMarketIndex range.
//
// Closed positions are removed from storage (or, when leverage was
// non-default, retained with BaseSize == 0); callers that want only
// open positions should keep their existing `pos.BaseSize.IsZero()`
// short-circuit to also skip the leverage-only config rows.
func (k Keeper) IterateAccountPositions(
	ctx context.Context,
	accountIdx uint64,
	cb func(types.AccountPosition) bool,
) error {
	rng := collections.NewPrefixedPairRange[uint64, uint32](accountIdx)
	iter, err := k.AccountPositions.Iterate(ctx, rng)
	if err != nil {
		return err
	}
	defer iter.Close()
	for ; iter.Valid(); iter.Next() {
		p, err := iter.Value()
		if err != nil {
			return err
		}
		p.NormalizeIntFields()
		if cb(p) {
			return nil
		}
	}
	return nil
}

// --------------------------------------------------------------------
//                           lifecycle primitives
// --------------------------------------------------------------------
//
// The three package-private lifecycle primitives below are the ONLY
// callers of setPosition / removePosition that emit a lifecycle event.
// They are deliberately unexported: external callers always go through
// a cohesive public method (ApplyFill / AdjustAllocatedMargin /
// ApplyFundingPayment / SetPositionLeverage / ClosePosition).

// openPosition persists `post` as a freshly opened position. Caller
// MUST guarantee `pre.BaseSize == 0` and `post.BaseSize != 0`; this
// primitive does not re-check (the cohesive caller already classified
// the transition). Allocates position_id, stamps CreatedAt, persists,
// and emits EventPositionOpened.
func (k Keeper) openPosition(ctx context.Context, post types.AccountPosition) (types.AccountPosition, error) {
	id, err := k.NextPositionIndex.Next(ctx)
	if err != nil {
		return types.AccountPosition{}, err
	}
	post.PositionId = id
	post.CreatedAt = sdk.UnwrapSDKContext(ctx).BlockTime().UnixMilli()
	post.NormalizeIntFields()
	if err := k.setPosition(ctx, post); err != nil {
		return types.AccountPosition{}, err
	}
	if err := k.emitPositionOpened(ctx, post); err != nil {
		return types.AccountPosition{}, err
	}
	return post, nil
}

// mutatePosition persists `post` as a same-side, in-place update of
// an open position. Caller MUST guarantee `pre.BaseSize != 0`,
// `post.BaseSize != 0`, and `sign(pre) == sign(post)`. Preserves
// position_id, persists, and emits EventPositionUpdated.
func (k Keeper) mutatePosition(ctx context.Context, pre, post types.AccountPosition) (types.AccountPosition, error) {
	post.PositionId = pre.PositionId
	if post.CreatedAt == 0 {
		post.CreatedAt = pre.CreatedAt
	}
	post.NormalizeIntFields()
	if err := k.setPosition(ctx, post); err != nil {
		return types.AccountPosition{}, err
	}
	if err := k.emitPositionUpdated(ctx, post); err != nil {
		return types.AccountPosition{}, err
	}
	return post, nil
}

// closePosition retires the row. Storage policy: the row is REMOVED
// from KV iff the user's leverage state is at the default (Cross +
// IMF == 0); otherwise the row is RETAINED as a leverage-only config
// row with BaseSize == 0 / position_id == 0, preserving
// {MarginMode, InitialMarginFraction} so the user's preference
// survives a close → reopen cycle.
//
// Emits EventPositionClosed with a post-close payload (BaseSize / EQ
// zeroed, position_id RETAINED on the event payload so the indexer
// can finalise the lifeline). Returns the **pre-close snapshot**
// unchanged so the cohesive caller can drain residual fields
// (allocated_margin etc.).
func (k Keeper) closePosition(ctx context.Context, pre types.AccountPosition) (types.AccountPosition, error) {
	retain := hasNonDefaultLeverage(pre)
	if retain {
		leverageOnly := types.AccountPosition{
			AccountIndex:             pre.AccountIndex,
			MarketIndex:              pre.MarketIndex,
			BaseSize:                 math.ZeroInt(),
			EntryQuote:               math.ZeroInt(),
			LastFundingRatePrefixSum: math.ZeroInt(),
			AllocatedMargin:          math.ZeroInt(),
			MarginMode:               pre.MarginMode,
			InitialMarginFraction:    pre.InitialMarginFraction,
		}
		if err := k.setPosition(ctx, leverageOnly); err != nil {
			return types.AccountPosition{}, err
		}
	} else {
		if err := k.removePosition(ctx, pre.AccountIndex, pre.MarketIndex); err != nil {
			return types.AccountPosition{}, err
		}
	}
	closedEvent := pre
	closedEvent.BaseSize = math.ZeroInt()
	closedEvent.EntryQuote = math.ZeroInt()
	if err := k.emitPositionClosed(ctx, closedEvent, !retain); err != nil {
		return types.AccountPosition{}, err
	}
	return pre, nil
}

// --------------------------------------------------------------------
//                       cohesive public API
// --------------------------------------------------------------------

// ApplyFill is the cohesive fill-application entry-point used by the
// trade engine. Owns the entire pipeline for one side of one fill:
//
//  1. Load `pre` via GetPosition.
//  2. Compute `fill = pre.ApplyFill(delta, price)` (pure math).
//  3. Bounds-check |post.BaseSize| / |post.EntryQuote| (returns
//     ErrPositionOutOfBounds; engine wraps to Maker/TakerInvalidPosition).
//  4. Classify the transition against `pre.BaseSize` /
//     `fill.Position.BaseSize` / `fill.SideFlipped` and dispatch:
//
//       - pre == 0, post != 0           → openPosition (new id, seeded
//                                          LastFundingRatePrefixSum =
//                                          fundingRatePrefixSum)
//       - pre != 0, post == 0           → closePosition (storage
//                                          reclaim + Closed event;
//                                          pre-close snapshot returned)
//       - SideFlipped                    → closePosition + openPosition
//                                          (Closed old id + Opened new
//                                          id; carries AllocatedMargin
//                                          / LastFundingRatePrefixSum
//                                          onto the residual leg so
//                                          isolated re-margin stays
//                                          arithmetically equivalent
//                                          to the pre-#91 codepath)
//       - same-side change               → mutatePosition (Updated;
//                                          position_id preserved)
//
// `fundingRatePrefixSum` is the market's current
// `MarketDetails.FundingRatePrefixSum`, passed in by the trade engine
// to avoid an x/account ↔ x/market dependency in this hot path
// (Cosmos late-bound keepers aren't visible to the trade engine's
// `accountKeeper` interface copy). Used only on the OPEN and FLIP
// transitions to seed the first post-open funding boundary; ignored
// on pure-close / same-side transitions.
//
// Returns a FillApplyResult populated for every transition; the
// engine keys downstream behaviour (isolated reconciliation, OI delta,
// risk check) off the result fields without re-reading position state.
func (k Keeper) ApplyFill(
	ctx context.Context,
	accIdx uint64,
	marketIdx uint32,
	price uint32,
	baseAmount uint64,
	sign int64,
	fundingRatePrefixSum math.Int,
) (types.FillApplyResult, error) {
	pre, err := k.GetPosition(ctx, accIdx, marketIdx)
	if err != nil {
		return types.FillApplyResult{}, err
	}
	delta := math.NewIntFromUint64(baseAmount).MulRaw(sign)
	fill := pre.ApplyFill(delta, price)

	if !withinPositionBounds(fill.Position.BaseSize, fill.Position.EntryQuote) {
		return types.FillApplyResult{}, types.ErrPositionOutOfBounds.Wrapf(
			"account %d market %d post-trade base=%s entry_quote=%s",
			accIdx, marketIdx, fill.Position.BaseSize.String(), fill.Position.EntryQuote.String())
	}

	if fundingRatePrefixSum.IsNil() {
		fundingRatePrefixSum = math.ZeroInt()
	}

	res := types.FillApplyResult{
		Old:         pre,
		RealizedPnL: fill.RealizedPnL,
		SideFlipped: fill.SideFlipped,
		OIDelta:     fill.Position.BaseSize.Abs().Sub(pre.BaseSize.Abs()).Int64(),
	}

	switch {
	case pre.BaseSize.IsZero():
		// Pure open. Seed LastFundingRatePrefixSum from the market's
		// current value (passed in by the engine) so the first
		// post-open SettlePositionFunding only charges from the open
		// boundary forward; pre-#91 the snapshot was kept up-to-date
		// on empty rows by the funding keeper, but with storage
		// reclamation that responsibility moved here.
		post := pre
		post.AccountIndex = accIdx
		post.MarketIndex = marketIdx
		post.BaseSize = fill.Position.BaseSize
		post.EntryQuote = fill.Position.EntryQuote
		post.LastFundingRatePrefixSum = fundingRatePrefixSum
		opened, err := k.openPosition(ctx, post)
		if err != nil {
			return types.FillApplyResult{}, err
		}
		res.New = opened
		return res, nil

	case fill.Position.BaseSize.IsZero():
		// Pure close. closePosition returns the pre-close snapshot;
		// the engine's isolated branch uses it to drain residual
		// allocated_margin back to cross collateral.
		closed, err := k.closePosition(ctx, pre)
		if err != nil {
			return types.FillApplyResult{}, err
		}
		res.New = closed
		res.Closed = true
		return res, nil

	case fill.SideFlipped:
		// Flip = close old + open new. The new lifeline carries the
		// user's pre-close AllocatedMargin and LastFundingRatePrefixSum
		// onto the residual leg so the isolated re-margin math
		// (`delta = posReq - (allocated + uPnL_new)`) has the same
		// starting allocated as the pre-#91 codepath; cross-mode just
		// inherits the snapshot which has no functional effect.
		closed, err := k.closePosition(ctx, pre)
		if err != nil {
			return types.FillApplyResult{}, err
		}
		residual := closed
		residual.AccountIndex = accIdx
		residual.MarketIndex = marketIdx
		residual.BaseSize = fill.Position.BaseSize
		residual.EntryQuote = fill.Position.EntryQuote
		residual.AllocatedMargin = closed.AllocatedMargin
		residual.LastFundingRatePrefixSum = closed.LastFundingRatePrefixSum
		// CreatedAt is re-stamped inside openPosition.
		residual.PositionId = 0
		residual.CreatedAt = 0
		opened, err := k.openPosition(ctx, residual)
		if err != nil {
			return types.FillApplyResult{}, err
		}
		res.New = opened
		return res, nil

	default:
		// Same-side increase / decrease.
		post := pre
		post.BaseSize = fill.Position.BaseSize
		post.EntryQuote = fill.Position.EntryQuote
		updated, err := k.mutatePosition(ctx, pre, post)
		if err != nil {
			return types.FillApplyResult{}, err
		}
		res.New = updated
		return res, nil
	}
}

// AdjustAllocatedMargin folds `delta` (signed) into the position's
// allocated_margin pool and emits EventPositionUpdated. The
// **canonical isolated-margin RMW** — replaces three separate
// mut-closure call sites (PnL/fee credit, improvement-fee debit,
// position_requirement rebalance) in the pre-#91 trade engine plus
// the AccountKeeper msg_server.UpdateMargin path.
//
// Asserts pre.BaseSize != 0: adjusting allocated_margin on a closed /
// leverage-only row would create a phantom balance with no position
// to back it, and is always a caller-side bug (the isolated close
// branch in the trade engine routes refunds straight to cross
// collateral instead of touching the position row).
func (k Keeper) AdjustAllocatedMargin(
	ctx context.Context,
	accIdx uint64,
	marketIdx uint32,
	delta math.Int,
) (types.AccountPosition, error) {
	pre, err := k.GetPosition(ctx, accIdx, marketIdx)
	if err != nil {
		return types.AccountPosition{}, err
	}
	if pre.BaseSize.IsZero() {
		return types.AccountPosition{}, types.ErrPositionLifecycleViolation.Wrapf(
			"AdjustAllocatedMargin: account %d market %d has no open position", accIdx, marketIdx)
	}
	if delta.IsNil() || delta.IsZero() {
		return pre, nil
	}
	post := pre
	post.AllocatedMargin = pre.AllocatedMargin.Add(delta)
	return k.mutatePosition(ctx, pre, post)
}

// ApplyFundingPayment is the cohesive funding-settlement RMW. Folds
// the per-position payment (`pos.BaseSize * (newPrefixSum -
// pos.LastFundingRatePrefixSum) / FundingRateTick`) into EntryQuote
// and snapshots the prefix sum. Emits EventPositionUpdated on a real
// write; short-circuits with no event on empty rows or zero-delta
// rounds.
//
// Closed / never-opened accounts have no funding obligation: x/trade's
// ApplyFill seeds LastFundingRatePrefixSum from the market's current
// value at open time, so this method is a no-op for BaseSize == 0
// rows and the funding keeper does NOT need to keep a snapshot in
// sync on empty rows (issue #91 storage reclamation).
func (k Keeper) ApplyFundingPayment(
	ctx context.Context,
	accIdx uint64,
	marketIdx uint32,
	newPrefixSum math.Int,
) (types.AccountPosition, error) {
	pre, err := k.GetPosition(ctx, accIdx, marketIdx)
	if err != nil {
		return types.AccountPosition{}, err
	}
	if pre.BaseSize.IsZero() {
		return pre, nil
	}
	if newPrefixSum.IsNil() {
		return pre, nil
	}
	deltaPrefix := newPrefixSum.Sub(pre.LastFundingRatePrefixSum)
	if deltaPrefix.IsZero() {
		return pre, nil
	}
	pay := pre.BaseSize.Mul(deltaPrefix).Quo(math.NewInt(perptypes.FundingRateTick))
	post := pre
	post.EntryQuote = pre.EntryQuote.Add(pay)
	post.LastFundingRatePrefixSum = newPrefixSum
	return k.mutatePosition(ctx, pre, post)
}

// SetPositionLeverage writes the leverage-only config row used to
// remember the user's preferred margin mode / IMF for the next open.
// Asserts there is no open position (`pre.BaseSize == 0`); the caller
// (msg_server UpdateLeverage) already enforces this from the user
// side, so a violation here would indicate a programming bug.
//
// Persists directly via `setPosition` (bypassing the lifecycle
// primitives — this is a configuration write, not a lifecycle
// transition) and emits EventPositionUpdated with `position_id == 0`
// so the indexer can distinguish it from open-position updates.
//
// No-op when both `pre` and the new (mode, imf) reduce to the default
// leverage state and no row was previously persisted; avoids creating
// empty default rows that would still occupy KV space.
func (k Keeper) SetPositionLeverage(
	ctx context.Context,
	accIdx uint64,
	marketIdx uint32,
	marginMode uint32,
	imf uint32,
) error {
	pre, err := k.GetPosition(ctx, accIdx, marketIdx)
	if err != nil {
		return err
	}
	if !pre.BaseSize.IsZero() {
		return types.ErrPositionLifecycleViolation.Wrapf(
			"SetPositionLeverage: account %d market %d has an open position (base_size=%s)",
			accIdx, marketIdx, pre.BaseSize.String())
	}
	post := pre
	post.AccountIndex = accIdx
	post.MarketIndex = marketIdx
	post.MarginMode = marginMode
	post.InitialMarginFraction = imf
	post.PositionId = 0
	post.NormalizeIntFields()

	if !hasNonDefaultLeverage(post) && !hasNonDefaultLeverage(pre) {
		return nil
	}
	if err := k.setPosition(ctx, post); err != nil {
		return err
	}
	return k.emitPositionUpdated(ctx, post)
}

// ClosePosition is the cohesive force-close entry-point: closes an
// open position outside the fill pipeline (e.g. for liquidation that
// bypasses ApplyFill, market expiry, IF / ADL absorbers). Asserts
// pre.BaseSize != 0 and routes through the same storage-reclamation
// + event-emission path as ApplyFill's close branch.
//
// Returns the **pre-close snapshot** (BaseSize still reflects the
// pre-close value; AllocatedMargin / EntryQuote / position_id are
// pre-close as well) so callers can drain residual fields.
func (k Keeper) ClosePosition(
	ctx context.Context,
	accIdx uint64,
	marketIdx uint32,
) (types.AccountPosition, error) {
	pre, err := k.GetPosition(ctx, accIdx, marketIdx)
	if err != nil {
		return types.AccountPosition{}, err
	}
	if pre.BaseSize.IsZero() {
		return types.AccountPosition{}, types.ErrPositionLifecycleViolation.Wrapf(
			"ClosePosition: account %d market %d has no open position", accIdx, marketIdx)
	}
	return k.closePosition(ctx, pre)
}
