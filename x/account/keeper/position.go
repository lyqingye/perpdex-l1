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

// Position lifecycle (issue #91):
//
// An AccountPosition row goes through three logical phases, mirrored
// one-to-one onto three typed events so off-chain indexers can rebuild
// the canonical per-position lifeline keyed by `position_id` from the
// event stream alone:
//
//   Open    : base_size 0 -> !=0      => EventPositionOpened   (new id)
//   Update  : base_size stays !=0     => EventPositionUpdated  (same id)
//   Close   : base_size !=0 -> 0      => EventPositionClosed   (final id)
//   Flip    : base_size sign crosses  => Closed (old id) + Opened (new id)
//
// `UpdatePosition` is the single RMW choke point that every external
// caller (x/trade applyPositionChange, x/funding SettlePositionFunding,
// x/account msg_server UpdateMargin / UpdateLeverage, x/liquidation)
// goes through; it inspects the pre / post snapshot and dispatches to
// the matching event automatically so callers stay agnostic to which
// transition they are about to trigger.
//
// `OpenPosition` and `ClosePosition` are explicit-intent helpers for
// the rare caller that already knows the lifecycle phase and wants the
// extra assertion-based safety net (e.g. liquidation paths that
// invariantly fully close a victim's position).
//
// Storage policy: a position is removed from the AccountPositions
// collection on close UNLESS it carries non-default leverage settings
// (non-Cross margin mode or a non-zero initial margin fraction), in
// which case the row is retained as a "leverage-only" config row so
// the user's preferred leverage survives the close → reopen cycle.
// `position_id` is zeroed on close.

// setPosition is the package-private write primitive used internally
// by the lifecycle dispatcher AND by InitGenesis to seed AccountPosition
// rows verbatim. It does NOT emit lifecycle events — emission is the
// dispatcher's job — so callers outside this file MUST use the cohesive
// mutator API (UpdatePosition / OpenPosition / ClosePosition).
func (k Keeper) setPosition(ctx context.Context, p types.AccountPosition) error {
	return k.AccountPositions.Set(ctx, collections.Join(p.AccountIndex, p.MarketIndex), p)
}

// emitPositionOpened fires EventPositionOpened with a full snapshot of
// the post-write row. Used on transition 0 -> !=0 and on the "open" leg
// of a flip.
func (k Keeper) emitPositionOpened(ctx context.Context, p types.AccountPosition) error {
	return sdk.UnwrapSDKContext(ctx).EventManager().EmitTypedEvent(&types.EventPositionOpened{
		Position: p,
	})
}

// emitPositionUpdated fires EventPositionUpdated with a full snapshot
// of the post-write row. Used on every same-side mutation and on
// leverage-only config writes against a base_size == 0 row.
func (k Keeper) emitPositionUpdated(ctx context.Context, p types.AccountPosition) error {
	return sdk.UnwrapSDKContext(ctx).EventManager().EmitTypedEvent(&types.EventPositionUpdated{
		Position: p,
	})
}

// emitPositionClosed fires EventPositionClosed with the final snapshot
// of the row at close time (base_size already zeroed, closed
// position_id retained). `deleted` reflects whether the row was
// removed from storage or kept as a leverage-only config row.
func (k Keeper) emitPositionClosed(ctx context.Context, p types.AccountPosition, deleted bool) error {
	return sdk.UnwrapSDKContext(ctx).EventManager().EmitTypedEvent(&types.EventPositionClosed{
		Position: p,
		Deleted:  deleted,
	})
}

// hasNonDefaultLeverage reports whether the row carries user-configured
// leverage state that must survive a close → reopen cycle. The
// "default" baseline matches the GetPosition auto-vivified zero record:
// Cross margin and IMF == 0 (i.e. fall back to the market default).
func hasNonDefaultLeverage(p types.AccountPosition) bool {
	return p.MarginMode != perptypes.CrossMargin || p.InitialMarginFraction != 0
}

// resetClosedRow zeroes the transient fields on `p` so a retained
// "leverage-only" row does not carry stale entry_quote / funding-rate
// snapshot / position_id state from the just-closed position. The
// caller MUST have already verified base_size == 0.
//
// `AllocatedMargin` is intentionally NOT reset here: the isolated-margin
// reconciliation in x/trade (calculateIsolatedMarginDelta's close
// branch) consumes the residual allocated_margin to credit it back to
// cross collateral. Resetting it inside the close branch would
// effectively erase the user's isolated margin pool — a regression on
// every isolated close. The follow-up `rebalanceIsolatedMargin` write
// drains it back to zero through the standard `UpdatePosition` path
// (which classifies the no-op vs leverage-only branches accordingly).
//
// Funding-rate prefix sum is reset because the next open will re-seed
// it from the market's current FundingRatePrefixSum; keeping the stale
// value would otherwise charge the new position for funding rounds it
// never held.
func resetClosedRow(p *types.AccountPosition) {
	p.EntryQuote = math.ZeroInt()
	p.LastFundingRatePrefixSum = math.ZeroInt()
	p.PositionId = 0
	p.TotalPositionTiedOrderCount = 0
	p.CreatedAt = 0
}

// GetPosition returns the position; an empty zero-valued one if absent.
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

// UpdatePosition is the canonical RMW dispatcher for AccountPosition.
// It loads the position (auto-vivifying a zero record when missing —
// same semantics as `GetPosition`), runs `mut` against a mutable
// pointer, then classifies the (pre, post) transition into one of:
//
//   - leverage-only no-op   : pre.base == 0, post.base == 0, no change
//                             on any non-default field. No write, no event.
//   - leverage-only write   : pre.base == 0, post.base == 0, some
//                             non-default field changed. Persist row with
//                             position_id == 0 + emit EventPositionUpdated.
//   - open                  : pre.base == 0, post.base != 0. Allocate a
//                             new position_id, stamp CreatedAt if unset,
//                             persist + emit EventPositionOpened.
//   - update (same side)    : pre.base != 0, post.base != 0, same sign.
//                             Persist + emit EventPositionUpdated. The
//                             position_id is preserved across this path.
//   - flip                  : pre.base != 0, post.base != 0, opposite
//                             sign. Emit EventPositionClosed for the
//                             old id, allocate a new id, reset
//                             CreatedAt, persist + emit
//                             EventPositionOpened.
//   - close                 : pre.base != 0, post.base == 0. Emit
//                             EventPositionClosed. Reset transient
//                             fields. Delete the row UNLESS the user has
//                             configured non-default leverage, in which
//                             case retain it as a leverage-only config
//                             row.
//
// If `mut` returns an error nothing is persisted (callers can short-
// circuit on bounds violations such as `errPositionOutOfBounds` in
// x/trade). The returned `AccountPosition` is the post-mutation value;
// on the close path the returned value carries base_size == 0 and the
// closed position_id retained (so callers can attribute downstream
// effects such as `EventOrderFilled.taker_position_id`).
func (k Keeper) UpdatePosition(
	ctx context.Context,
	accIdx uint64,
	marketIdx uint32,
	mut func(*types.AccountPosition) error,
) (types.AccountPosition, error) {
	pre, err := k.GetPosition(ctx, accIdx, marketIdx)
	if err != nil {
		return types.AccountPosition{}, err
	}
	// Copy by value so post-mutator inspection compares against the
	// untouched pre-state. AccountIndex / MarketIndex are stamped
	// because GetPosition's not-found branch returns them but a
	// future change might omit them.
	post := pre
	post.AccountIndex = accIdx
	post.MarketIndex = marketIdx
	if err := mut(&post); err != nil {
		return types.AccountPosition{}, err
	}
	post.NormalizeIntFields()

	preZero := pre.BaseSize.IsZero()
	postZero := post.BaseSize.IsZero()

	switch {
	case preZero && postZero:
		// Leverage-only write (or a degenerate no-op). Persist only
		// if something on the row actually changed so we don't burn
		// IO on no-op funding settlements against an empty position.
		if positionRowsEqual(pre, post) {
			return post, nil
		}
		// position_id stays 0 — no open position yet.
		post.PositionId = 0
		if err := k.setPosition(ctx, post); err != nil {
			return types.AccountPosition{}, err
		}
		if err := k.emitPositionUpdated(ctx, post); err != nil {
			return types.AccountPosition{}, err
		}
		return post, nil

	case preZero && !postZero:
		// Open. Allocate a fresh id, stamp CreatedAt if the caller
		// did not, persist + emit.
		id, err := k.NextPositionIndex.Next(ctx)
		if err != nil {
			return types.AccountPosition{}, err
		}
		post.PositionId = id
		if post.CreatedAt == 0 {
			post.CreatedAt = sdk.UnwrapSDKContext(ctx).BlockTime().UnixMilli()
		}
		if err := k.setPosition(ctx, post); err != nil {
			return types.AccountPosition{}, err
		}
		if err := k.emitPositionOpened(ctx, post); err != nil {
			return types.AccountPosition{}, err
		}
		return post, nil

	case !preZero && postZero:
		// Close. Snapshot the final row (with the closed
		// position_id retained on the event payload), reset
		// transient fields, then either delete or retain as a
		// leverage-only config row.
		closedSnapshot := post
		closedSnapshot.PositionId = pre.PositionId

		retain := hasNonDefaultLeverage(post)
		resetClosedRow(&post)
		if retain {
			if err := k.setPosition(ctx, post); err != nil {
				return types.AccountPosition{}, err
			}
		} else {
			if err := k.AccountPositions.Remove(ctx, collections.Join(accIdx, marketIdx)); err != nil {
				return types.AccountPosition{}, err
			}
		}
		if err := k.emitPositionClosed(ctx, closedSnapshot, !retain); err != nil {
			return types.AccountPosition{}, err
		}
		// Return the closed snapshot so callers (and tests) see the
		// final pre-close shape including the retained position_id.
		return closedSnapshot, nil
	}

	// pre.base != 0 && post.base != 0: either same-side update or flip.
	if pre.BaseSize.IsNegative() != post.BaseSize.IsNegative() {
		// Flip: close the old position, then open a new one with a
		// fresh id. We surface this as two events in the canonical
		// close → open order so indexers can finalise the old
		// lifeline before opening a new one.
		closedSnapshot := pre
		closedSnapshot.BaseSize = math.ZeroInt()
		closedSnapshot.EntryQuote = math.ZeroInt()
		// PnL routing on the closing leg lives in x/trade's
		// applyAccount; here we just record the final shape.
		if err := k.emitPositionClosed(ctx, closedSnapshot, false /* deleted=false; row overwritten */); err != nil {
			return types.AccountPosition{}, err
		}
		id, err := k.NextPositionIndex.Next(ctx)
		if err != nil {
			return types.AccountPosition{}, err
		}
		post.PositionId = id
		post.CreatedAt = sdk.UnwrapSDKContext(ctx).BlockTime().UnixMilli()
		if err := k.setPosition(ctx, post); err != nil {
			return types.AccountPosition{}, err
		}
		if err := k.emitPositionOpened(ctx, post); err != nil {
			return types.AccountPosition{}, err
		}
		return post, nil
	}

	// Same-side update. position_id is preserved.
	post.PositionId = pre.PositionId
	if post.CreatedAt == 0 {
		post.CreatedAt = pre.CreatedAt
	}
	if err := k.setPosition(ctx, post); err != nil {
		return types.AccountPosition{}, err
	}
	if err := k.emitPositionUpdated(ctx, post); err != nil {
		return types.AccountPosition{}, err
	}
	return post, nil
}

// OpenPosition is the explicit-intent helper for callers that already
// know they are transitioning from a zero base to a non-zero base. It
// asserts `pre.BaseSize.IsZero()` AND `post.BaseSize.IsZero() == false`
// post-mutation; otherwise returns ErrPositionLifecycleViolation. Use
// when the lifecycle phase is statically known so a programming bug
// surfaces loudly instead of silently routing through UpdatePosition's
// dispatcher.
func (k Keeper) OpenPosition(
	ctx context.Context,
	accIdx uint64,
	marketIdx uint32,
	mut func(*types.AccountPosition) error,
) (types.AccountPosition, error) {
	pre, err := k.GetPosition(ctx, accIdx, marketIdx)
	if err != nil {
		return types.AccountPosition{}, err
	}
	if !pre.BaseSize.IsZero() {
		return types.AccountPosition{}, types.ErrPositionLifecycleViolation.Wrapf(
			"OpenPosition: account %d market %d already has base_size=%s; use UpdatePosition",
			accIdx, marketIdx, pre.BaseSize.String())
	}
	post, err := k.UpdatePosition(ctx, accIdx, marketIdx, mut)
	if err != nil {
		return types.AccountPosition{}, err
	}
	if post.BaseSize.IsZero() {
		return types.AccountPosition{}, types.ErrPositionLifecycleViolation.Wrapf(
			"OpenPosition: account %d market %d mut left base_size=0", accIdx, marketIdx)
	}
	return post, nil
}

// ClosePosition is the explicit-intent helper for callers that already
// know the position must be fully closed (e.g. liquidation full
// close-out). It zeroes BaseSize / EntryQuote through the standard
// UpdatePosition dispatcher so the close event + retention logic stays
// in one place. Returns ErrPositionLifecycleViolation if the position
// is already empty (so accidental double-close surfaces).
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
			"ClosePosition: account %d market %d has base_size=0", accIdx, marketIdx)
	}
	return k.UpdatePosition(ctx, accIdx, marketIdx, func(p *types.AccountPosition) error {
		p.BaseSize = math.ZeroInt()
		p.EntryQuote = math.ZeroInt()
		return nil
	})
}

// SetPositionLeverage flips a position's `MarginMode` and
// `InitialMarginFraction`. Used by Msg.UpdateLeverage; the caller
// has already validated the position is empty and the imf falls
// inside [market_min, MarginTick].
//
// Routes through UpdatePosition so the "leverage-only write" branch
// fires EventPositionUpdated (no position_id allocation: this is a
// configuration write, not an open).
func (k Keeper) SetPositionLeverage(
	ctx context.Context,
	accIdx uint64,
	marketIdx uint32,
	marginMode uint32,
	imf uint32,
) error {
	_, err := k.UpdatePosition(ctx, accIdx, marketIdx, func(p *types.AccountPosition) error {
		p.MarginMode = marginMode
		p.InitialMarginFraction = imf
		return nil
	})
	return err
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

// positionRowsEqual reports whether two AccountPosition rows would
// produce the same persisted bytes. Used by the leverage-only branch
// of UpdatePosition to skip a redundant write + event when the mutator
// did not actually mutate anything (e.g. a SettlePositionFunding call
// against a position that is already current on funding).
func positionRowsEqual(a, b types.AccountPosition) bool {
	if a.AccountIndex != b.AccountIndex || a.MarketIndex != b.MarketIndex {
		return false
	}
	if a.PositionId != b.PositionId {
		return false
	}
	if !intEqualOrBothZero(a.BaseSize, b.BaseSize) ||
		!intEqualOrBothZero(a.EntryQuote, b.EntryQuote) ||
		!intEqualOrBothZero(a.LastFundingRatePrefixSum, b.LastFundingRatePrefixSum) ||
		!intEqualOrBothZero(a.AllocatedMargin, b.AllocatedMargin) {
		return false
	}
	if a.MarginMode != b.MarginMode || a.InitialMarginFraction != b.InitialMarginFraction {
		return false
	}
	if a.TotalOrderCount != b.TotalOrderCount ||
		a.TotalPositionTiedOrderCount != b.TotalPositionTiedOrderCount {
		return false
	}
	if a.CreatedAt != b.CreatedAt {
		return false
	}
	return true
}

// intEqualOrBothZero treats nil math.Int as math.ZeroInt() (mirroring
// `NormalizeIntFields` semantics) so the equality check is robust to
// the two equivalent representations of zero we may see in practice.
func intEqualOrBothZero(a, b math.Int) bool {
	az := a.IsNil() || a.IsZero()
	bz := b.IsNil() || b.IsZero()
	if az && bz {
		return true
	}
	if az != bz {
		return false
	}
	return a.Equal(b)
}
