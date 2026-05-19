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

// Position keeper surface (issue #91). x/account is the sole owner of
// AccountPosition writes; external callers go through the cohesive
// public methods below — never a mut closure. The three package-
// private lifecycle primitives (openPosition / mutatePosition /
// closePosition) only exist so the cohesive methods can share the
// persistence + event-emission plumbing. See spec/events/account.md
// for the full lifecycle invariants and event contract.

// emptyPosition is the canonical "no open position" record for
// (accIdx, marketIdx): the auto-vivified zero used by GetPosition and
// the base shape for a leverage-only row in closePosition.
func emptyPosition(accIdx uint64, marketIdx uint32) types.AccountPosition {
	return types.AccountPosition{
		AccountIndex:             accIdx,
		MarketIndex:              marketIdx,
		BaseSize:                 math.ZeroInt(),
		EntryQuote:               math.ZeroInt(),
		LastFundingRatePrefixSum: math.ZeroInt(),
		AllocatedMargin:          math.ZeroInt(),
		MarginMode:               perptypes.CrossMargin,
	}
}

// hasNonDefaultLeverage reports whether the row carries user-configured
// leverage state worth surviving a close → reopen cycle. "Default" is
// Cross + IMF == 0 (fall back to the market default).
func hasNonDefaultLeverage(p types.AccountPosition) bool {
	return p.MarginMode != perptypes.CrossMargin || p.InitialMarginFraction != 0
}

// withinPositionBounds enforces |position| ≤ MaxPositionSize and
// |entry_quote| ≤ MaxEntryQuote (the per-market wire encoding caps).
func withinPositionBounds(position, entryQuote math.Int) bool {
	return position.Abs().LTE(math.NewIntFromUint64(perptypes.MaxPositionSize)) &&
		entryQuote.Abs().LTE(math.NewIntFromUint64(perptypes.MaxEntryQuote))
}

func (k Keeper) setPosition(ctx context.Context, p types.AccountPosition) error {
	return k.AccountPositions.Set(ctx, collections.Join(p.AccountIndex, p.MarketIndex), p)
}

func (k Keeper) removePosition(ctx context.Context, accIdx uint64, marketIdx uint32) error {
	return k.AccountPositions.Remove(ctx, collections.Join(accIdx, marketIdx))
}

func (k Keeper) emitOpened(ctx context.Context, p types.AccountPosition) error {
	return sdk.UnwrapSDKContext(ctx).EventManager().EmitTypedEvent(&types.EventPositionOpened{Position: p})
}

func (k Keeper) emitUpdated(ctx context.Context, p types.AccountPosition) error {
	return sdk.UnwrapSDKContext(ctx).EventManager().EmitTypedEvent(&types.EventPositionUpdated{Position: p})
}

func (k Keeper) emitClosed(ctx context.Context, p types.AccountPosition, deleted bool) error {
	return sdk.UnwrapSDKContext(ctx).EventManager().EmitTypedEvent(&types.EventPositionClosed{Position: p, Deleted: deleted})
}

// GetPosition returns the position; an empty zero record (BaseSize ==
// 0, NOT persisted) if absent. Callers use BaseSize.IsZero() as the
// "no open position" sentinel and also to skip leverage-only config
// rows when iterating.
func (k Keeper) GetPosition(ctx context.Context, accIdx uint64, marketIdx uint32) (types.AccountPosition, error) {
	p, err := k.AccountPositions.Get(ctx, collections.Join(accIdx, marketIdx))
	if errors.Is(err, collections.ErrNotFound) {
		return emptyPosition(accIdx, marketIdx), nil
	}
	if err != nil {
		return types.AccountPosition{}, err
	}
	p.NormalizeIntFields()
	return p, nil
}

// IterateAccountPositions walks every persisted row owned by
// `accountIdx`. The callback returns `true` to stop early. Per-account
// driver for risk / liquidation / funding loops; callers that want
// only open positions should keep their `pos.BaseSize.IsZero()`
// short-circuit to also skip leverage-only config rows.
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

// openPosition persists `post` as a freshly opened position. Caller
// MUST guarantee pre.BaseSize == 0 and post.BaseSize != 0; this
// primitive does not re-check. Allocates position_id, stamps
// CreatedAt, emits EventPositionOpened.
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
	if err := k.emitOpened(ctx, post); err != nil {
		return types.AccountPosition{}, err
	}
	return post, nil
}

// mutatePosition persists `post` as a same-side, in-place update of
// an open position. Caller MUST guarantee pre and post are both open
// with the same sign. Preserves position_id, emits
// EventPositionUpdated.
func (k Keeper) mutatePosition(ctx context.Context, pre, post types.AccountPosition) (types.AccountPosition, error) {
	post.PositionId = pre.PositionId
	if post.CreatedAt == 0 {
		post.CreatedAt = pre.CreatedAt
	}
	post.NormalizeIntFields()
	if err := k.setPosition(ctx, post); err != nil {
		return types.AccountPosition{}, err
	}
	if err := k.emitUpdated(ctx, post); err != nil {
		return types.AccountPosition{}, err
	}
	return post, nil
}

// closePosition retires `pre`. Storage policy: REMOVE on default
// leverage, RETAIN as a leverage-only row otherwise (preserves the
// user's preferred margin_mode / IMF across close → reopen). Emits
// EventPositionClosed; returns the unchanged pre-close snapshot so
// callers can drain residual fields (allocated_margin etc.).
func (k Keeper) closePosition(ctx context.Context, pre types.AccountPosition) (types.AccountPosition, error) {
	retain := hasNonDefaultLeverage(pre)
	if retain {
		row := emptyPosition(pre.AccountIndex, pre.MarketIndex)
		row.MarginMode = pre.MarginMode
		row.InitialMarginFraction = pre.InitialMarginFraction
		if err := k.setPosition(ctx, row); err != nil {
			return types.AccountPosition{}, err
		}
	} else if err := k.removePosition(ctx, pre.AccountIndex, pre.MarketIndex); err != nil {
		return types.AccountPosition{}, err
	}
	payload := pre
	payload.BaseSize, payload.EntryQuote = math.ZeroInt(), math.ZeroInt()
	if err := k.emitClosed(ctx, payload, !retain); err != nil {
		return types.AccountPosition{}, err
	}
	return pre, nil
}

// ApplyFill is the cohesive fill-application entry-point. Computes
// fill math, classifies the transition (open / mutate / close / flip),
// persists through the matching lifecycle primitive, emits exactly
// one (or two, for flip) lifecycle event(s), and returns the
// pre/post snapshots + realized PnL + OI delta the trade engine
// keys downstream behaviour off.
//
// `baseDelta` is the SIGNED base amount this side trades (positive =
// buys base, negative = sells); the trade engine derives it from the
// fill's BaseAmount + IsTakerAsk.
//
// `fundingRatePrefixSum` is the market's current
// `MarketDetails.FundingRatePrefixSum`, threaded in by the caller
// because x/account doesn't hold a marketKeeper handle visible to
// the trade engine's accountKeeper interface copy. Used only on the
// open / flip transitions to seed the new lifeline's first funding
// boundary.
//
// Out-of-bounds post-trade state surfaces as ErrPositionOutOfBounds,
// which the trade engine wraps into Maker/TakerInvalidPosition.
func (k Keeper) ApplyFill(
	ctx context.Context,
	accIdx uint64,
	marketIdx uint32,
	price uint32,
	baseDelta math.Int,
	fundingRatePrefixSum math.Int,
) (types.FillApplyResult, error) {
	pre, err := k.GetPosition(ctx, accIdx, marketIdx)
	if err != nil {
		return types.FillApplyResult{}, err
	}
	fill := pre.ApplyFill(baseDelta, price)
	if !withinPositionBounds(fill.Position.BaseSize, fill.Position.EntryQuote) {
		return types.FillApplyResult{}, types.ErrPositionOutOfBounds.Wrapf(
			"account %d market %d post-trade base=%s entry_quote=%s",
			accIdx, marketIdx, fill.Position.BaseSize, fill.Position.EntryQuote)
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

	// openAt opens a new lifeline by inheriting MarginMode / IMF /
	// AllocatedMargin from `source` and stamping in the post-fill
	// BaseSize / EntryQuote + the new funding boundary. openPosition
	// itself overwrites PositionId and CreatedAt, so we don't bother
	// zeroing them here.
	openAt := func(source types.AccountPosition) (types.AccountPosition, error) {
		post := source
		post.AccountIndex = accIdx
		post.MarketIndex = marketIdx
		post.BaseSize = fill.Position.BaseSize
		post.EntryQuote = fill.Position.EntryQuote
		post.LastFundingRatePrefixSum = fundingRatePrefixSum
		return k.openPosition(ctx, post)
	}

	switch {
	case pre.BaseSize.IsZero():
		res.New, err = openAt(pre)

	case fill.Position.BaseSize.IsZero():
		// Pure close. closePosition returns the pre-close snapshot;
		// the engine's isolated branch drains residual allocated_margin
		// from it straight to cross collateral.
		res.New, err = k.closePosition(ctx, pre)
		res.Closed = true

	case fill.SideFlipped:
		// Flip = close old + open new. The new leg inherits pre's
		// MarginMode / IMF / AllocatedMargin (via openAt's source) so
		// isolated re-margin math stays arithmetically equivalent to
		// the pre-#91 codepath.
		if _, err = k.closePosition(ctx, pre); err != nil {
			return types.FillApplyResult{}, err
		}
		res.New, err = openAt(pre)

	default:
		// Same-side change.
		post := pre
		post.BaseSize = fill.Position.BaseSize
		post.EntryQuote = fill.Position.EntryQuote
		res.New, err = k.mutatePosition(ctx, pre, post)
	}
	if err != nil {
		return types.FillApplyResult{}, err
	}
	return res, nil
}

// AdjustAllocatedMargin folds `delta` (signed) into the position's
// allocated_margin pool and emits EventPositionUpdated. Canonical RMW
// for the isolated-margin pool: used by the trade engine's three-step
// isolated reconciliation (PnL/fee credit, improvement-fee debit,
// position_requirement rebalance) and by the msg_server UpdateMargin
// path. Asserts pre.BaseSize != 0 — adjusting allocated_margin on a
// closed row would create a phantom balance with no position to back it.
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
// `pay = BaseSize * (newPrefixSum - LastFundingRatePrefixSum) /
// FundingRateTick` into EntryQuote and snapshots the new prefix sum;
// emits EventPositionUpdated on a real write. No-op (no event) on
// empty rows, nil prefix sums, or zero-delta rounds — closed accounts
// have no funding obligation, and the next ApplyFill re-seeds the
// snapshot from the market's current value.
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
	if pre.BaseSize.IsZero() || newPrefixSum.IsNil() {
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

// SetPositionLeverage writes (or clears) the leverage-only config row
// remembering the user's preferred margin mode / IMF for the next
// open. Asserts there is no open position. Emits EventPositionUpdated
// with position_id == 0 so indexers can tell a config update apart
// from an open-position update.
//
// Storage policy:
//
//   - default → default : no-op (nothing to persist).
//   - non-default → default: REMOVE the row (release KV space; the
//     auto-vivified GetPosition response already covers the default).
//   - * → non-default : write / update the leverage-only row.
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
			accIdx, marketIdx, pre.BaseSize)
	}
	post := emptyPosition(accIdx, marketIdx)
	post.MarginMode = marginMode
	post.InitialMarginFraction = imf

	preDefault := !hasNonDefaultLeverage(pre)
	postDefault := !hasNonDefaultLeverage(post)
	switch {
	case preDefault && postDefault:
		return nil
	case !preDefault && postDefault:
		if err := k.removePosition(ctx, accIdx, marketIdx); err != nil {
			return err
		}
	default:
		if err := k.setPosition(ctx, post); err != nil {
			return err
		}
	}
	return k.emitUpdated(ctx, post)
}

// ClosePosition is the cohesive force-close entry-point: closes an
// open position outside the fill pipeline (liquidation paths that
// bypass ApplyFill, market expiry, IF / ADL absorbers). Returns the
// pre-close snapshot so callers can drain residual fields.
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
