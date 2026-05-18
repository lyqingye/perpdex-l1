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

// Position lifecycle API (issue #91).
//
// AccountPosition writes are split into THREE explicit, narrow methods.
// There is intentionally no all-in-one RMW entrypoint: callers MUST
// decide which lifecycle phase the mutation belongs to, so the
// indexer-facing event stream stays unambiguous and so a buggy caller
// surfaces as ErrPositionLifecycleViolation instead of silently
// running through a generic dispatcher.
//
//   OpenPosition  : create a brand-new open position. Asserts there is
//                   no open position (pre.BaseSize == 0). Mut must
//                   leave BaseSize != 0. Allocates position_id and
//                   stamps CreatedAt. Emits EventPositionOpened.
//
//   MutatePosition: RMW on an existing open position. Asserts
//                   pre.BaseSize != 0 AND post.BaseSize != 0 with the
//                   SAME sign (no zeroing, no flips). Emits
//                   EventPositionUpdated. position_id is preserved.
//
//   ClosePosition : closes an open position. Asserts pre.BaseSize != 0.
//                   Removes the row from KV (or retains it as a
//                   leverage-only config row iff the user configured
//                   non-default leverage). Returns the pre-close
//                   snapshot so the caller can drain residual fields
//                   (allocated_margin etc.) on its own. Emits
//                   EventPositionClosed.
//
// Side flip (e.g. a reverse fill that crosses the zero line) is a
// caller-orchestrated `ClosePosition` followed by `OpenPosition`. The
// trade engine `applyPositionChange` is the canonical site that does
// this dispatch — it has all the fill math (`ApplyFill`) needed to
// determine the lifecycle phase up front.
//
// Leverage configuration writes (`SetPositionLeverage` /
// `MsgUpdateLeverage`) intentionally do NOT go through any of the
// lifecycle methods because they're not a lifecycle transition.
// `SetPositionLeverage` writes a leverage-only row directly via
// `setPosition` and emits `EventPositionUpdated` so indexers still see
// the configuration change.

// setPosition is the package-private write primitive used by genesis
// restore, the lifecycle dispatchers above, and `SetPositionLeverage`.
// It does NOT emit any event — emission is the caller's job — so
// external callers MUST use one of `OpenPosition` / `MutatePosition` /
// `ClosePosition` / `SetPositionLeverage`.
func (k Keeper) setPosition(ctx context.Context, p types.AccountPosition) error {
	return k.AccountPositions.Set(ctx, collections.Join(p.AccountIndex, p.MarketIndex), p)
}

// removePosition is the package-private delete primitive. Used by
// ClosePosition's "default-leverage" branch to release KV storage.
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

// GetPosition returns the position; an empty zero-valued one if absent.
// An empty zero-valued response carries `BaseSize == 0` and is used by
// callers as the "no open position" sentinel; the auto-vivified record
// is NOT persisted unless the caller subsequently routes through a
// lifecycle method.
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

// OpenPosition opens a brand-new position. The caller's mutator is
// invoked against a pre-populated `AccountPosition` whose leverage
// fields (`MarginMode`, `InitialMarginFraction`) reflect any
// pre-existing leverage-only config row (so the user's preferred
// leverage carries into the freshly opened position). The mutator
// MUST leave `BaseSize != 0`; otherwise returns
// `ErrPositionLifecycleViolation`.
//
// On success:
//   - allocates a fresh `position_id` (>= 1).
//   - stamps `CreatedAt` with the current block time.
//   - persists the row.
//   - emits `EventPositionOpened`.
//
// Returns the post-write snapshot.
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
			"OpenPosition: account %d market %d already open (base_size=%s, position_id=%d)",
			accIdx, marketIdx, pre.BaseSize.String(), pre.PositionId)
	}
	post := pre
	post.AccountIndex = accIdx
	post.MarketIndex = marketIdx
	if err := mut(&post); err != nil {
		return types.AccountPosition{}, err
	}
	post.NormalizeIntFields()
	if post.BaseSize.IsZero() {
		return types.AccountPosition{}, types.ErrPositionLifecycleViolation.Wrapf(
			"OpenPosition: account %d market %d mut left base_size=0; use SetPositionLeverage for leverage-only writes",
			accIdx, marketIdx)
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

// MutatePosition runs read-modify-write against an existing open
// position. Asserts:
//   - pre.BaseSize != 0  (there is an open position; otherwise use
//                         OpenPosition or SetPositionLeverage).
//   - post.BaseSize != 0 (mutator may not zero the size; use
//                         ClosePosition instead).
//   - sign(post) == sign(pre) (mutator may not flip the side; use
//                              ClosePosition + OpenPosition for flips).
//
// Violations surface as `ErrPositionLifecycleViolation` so a buggy
// caller fails loudly. position_id is preserved across the mutation.
// Persists the row and emits `EventPositionUpdated`.
//
// Used by:
//   - x/trade for same-side increases / decreases.
//   - x/trade isolated margin for allocated_margin reconciliation.
//   - x/funding for entry_quote / LastFundingRatePrefixSum updates.
//   - x/account msg_server UpdateMargin.
func (k Keeper) MutatePosition(
	ctx context.Context,
	accIdx uint64,
	marketIdx uint32,
	mut func(*types.AccountPosition) error,
) (types.AccountPosition, error) {
	pre, err := k.GetPosition(ctx, accIdx, marketIdx)
	if err != nil {
		return types.AccountPosition{}, err
	}
	if pre.BaseSize.IsZero() {
		return types.AccountPosition{}, types.ErrPositionLifecycleViolation.Wrapf(
			"MutatePosition: account %d market %d has no open position", accIdx, marketIdx)
	}
	post := pre
	post.AccountIndex = accIdx
	post.MarketIndex = marketIdx
	if err := mut(&post); err != nil {
		return types.AccountPosition{}, err
	}
	post.NormalizeIntFields()
	if post.BaseSize.IsZero() {
		return types.AccountPosition{}, types.ErrPositionLifecycleViolation.Wrapf(
			"MutatePosition: account %d market %d mut zeroed base_size; use ClosePosition",
			accIdx, marketIdx)
	}
	if pre.BaseSize.IsNegative() != post.BaseSize.IsNegative() {
		return types.AccountPosition{}, types.ErrPositionLifecycleViolation.Wrapf(
			"MutatePosition: account %d market %d mut flipped sign (pre=%s, post=%s); use ClosePosition + OpenPosition",
			accIdx, marketIdx, pre.BaseSize.String(), post.BaseSize.String())
	}
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

// ClosePosition closes an existing open position. Asserts
// `pre.BaseSize != 0`.
//
// Storage policy: the row is REMOVED from KV iff the user's leverage
// state is at the default (Cross margin + IMF == 0). When the user
// configured non-default leverage (e.g. Isolated) the row is RETAINED
// as a "leverage-only config" row with `BaseSize = 0, position_id = 0`,
// preserving `MarginMode` / `InitialMarginFraction` so they survive a
// close → re-open cycle.
//
// Returns the **pre-close snapshot** (BaseSize still reflects the
// pre-close value; AllocatedMargin / EntryQuote / position_id are
// pre-close as well). Callers (x/trade isolated reconciliation) use
// the returned snapshot to drain residual allocated_margin back to
// cross collateral.
//
// `EventPositionClosed` is fired with a post-close payload (BaseSize
// zeroed, position_id RETAINED on the event payload — sat indexers can
// finalise the lifeline). The event's `deleted` field reports whether
// the row was removed (`true`) or retained as leverage-only (`false`).
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

	retain := hasNonDefaultLeverage(pre)
	if retain {
		leverageOnly := types.AccountPosition{
			AccountIndex:             accIdx,
			MarketIndex:              marketIdx,
			BaseSize:                 math.ZeroInt(),
			EntryQuote:               math.ZeroInt(),
			LastFundingRatePrefixSum: math.ZeroInt(),
			AllocatedMargin:          math.ZeroInt(),
			MarginMode:               pre.MarginMode,
			InitialMarginFraction:    pre.InitialMarginFraction,
			// position_id, total_*_count, created_at all zero.
		}
		if err := k.setPosition(ctx, leverageOnly); err != nil {
			return types.AccountPosition{}, err
		}
	} else {
		if err := k.removePosition(ctx, accIdx, marketIdx); err != nil {
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

// SetPositionLeverage writes the leverage-only config row used to
// remember the user's preferred margin mode / IMF for the next open.
// Asserts there is no open position (pre.BaseSize == 0); the caller
// (msg_server UpdateLeverage) already enforces this from the user
// side, so a violation here would indicate a programming bug.
//
// Persists the row directly (bypassing OpenPosition / MutatePosition /
// ClosePosition — this is a configuration write, not a lifecycle
// transition) and emits `EventPositionUpdated` with `position_id == 0`
// so the indexer can distinguish it from open-position updates.
//
// If the new leverage settings reduce to the default (Cross + IMF=0)
// AND there is no prior leverage-only row, this is effectively a no-op
// and no event is emitted (avoids creating an empty row for the
// default state).
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

	// If both pre and post collapse to the default leverage state AND
	// no row existed (or the row would be a duplicate), skip the write
	// + event so the indexer doesn't see noise from no-op transitions
	// like "Cross+0 -> Cross+0".
	if !hasNonDefaultLeverage(post) && !hasNonDefaultLeverage(pre) {
		// Nothing to record.
		return nil
	}
	if err := k.setPosition(ctx, post); err != nil {
		return err
	}
	return k.emitPositionUpdated(ctx, post)
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
