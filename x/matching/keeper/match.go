package keeper

import (
	"context"
	"strconv"

	sdk "github.com/cosmos/cosmos-sdk/types"

	perptypes "github.com/perpdex/perpdex-l1/types"
	"github.com/perpdex/perpdex-l1/x/matching/types"
	orderbooktypes "github.com/perpdex/perpdex-l1/x/orderbook/types"
	tradekeeper "github.com/perpdex/perpdex-l1/x/trade/keeper"
	tradetypes "github.com/perpdex/perpdex-l1/x/trade/types"
)

// fillStep classifies the outcome of one matching iteration. It is the
// vocabulary `trySingleFill` uses to communicate back to its outer
// loop without coupling either side to the loop's specific state
// machine (residue accounting, fill counter, cancel reason, etc.).
type fillStep uint8

const (
	// fillStepBreak: terminal loop condition reached (book empty,
	// price unreachable, self-trade, reduce-only invariant breached
	// against the taker's own state). The outer loop must exit.
	fillStepBreak fillStep = iota

	// fillStepContinue: a bad maker was evicted on the OUTER ctx;
	// the outer loop should peek the next maker without changing
	// fill counters or taker residue.
	fillStepContinue

	// fillStepFilled: a successful fill was committed via writeCache.
	// `base` and `price` describe the fill the outer loop should
	// account for.
	fillStepFilled

	// fillStepTakerAbort: recoverable taker error (Lighter parity:
	// `cancel_taker_order`). The outer loop should preserve already-
	// committed fills, terminate the loop, and force-cancel the
	// residue rather than rest it on the book.
	fillStepTakerAbort
)

// fillResult is the value returned from `trySingleFill`. `base` and
// `price` are only meaningful when `step == fillStepFilled`.
type fillResult struct {
	step  fillStep
	base  uint64
	price uint32
}

// applyFn is the per-fill closure the outer loop injects into
// `trySingleFill`. It receives the cached ctx (so an apply failure
// can be cleanly rolled back without touching the outer write set),
// the maker book entry being matched, and the trade base. The closure
// decides how to construct the matching engine's PerpFill / SpotFill
// — user matchOrder injects the standard maker/taker fees;
// matchLiquidationLoop injects the liquidation-specific PerpFill that
// carries ZeroPrice + LiquidationFeeBps + LiquidationFeeRecipient +
// SkipMakerRiskCheck.
//
// `applyFn` MUST NOT call `bookKeeper.FillMakerOrder` itself —
// `trySingleFill` owns the orderbook update so the cacheCtx unwinds
// cleanly when ApplyPerpsMatching errs after the fill record is
// already written.
type applyFn func(cacheCtx context.Context, best orderbooktypes.OrderBookEntry, tradeBase uint64) error

// trySingleFill is the shared inner kernel of one matching iteration:
// peek the best opposite, run all pre-fill validations
// (price-reachable, self-trade, GTT expiry eviction, reduce-only
// bounds for both sides, trade-base sizing), wrap the apply + maker
// orderbook update in an isolated cacheCtx, and classify the outcome
// into a `fillStep`.
//
// Two callers share this kernel:
//
//   - `matchOrder`: services user-driven CreateOrder / ModifyOrder
//     flows. It tracks RemainingBaseAmount, the fills counter, and
//     emits an OrderFill event per successful iteration.
//   - `matchLiquidationLoop`: services system-issued
//     `LIQUIDATION_ORDER + IOC + reduce_only` taker. It additionally
//     re-checks the victim's health after each fill (Lighter
//     `is_not_in_liquidation_and_is_liquidation_order` short-circuit)
//     and never rests the synthetic taker on the book.
//
// The helper does NOT mutate `taker.RemainingBaseAmount`, the fill
// counter, or the outer status. The outer loops own those updates
// because their state machines diverge (the user residue may be
// IOC-cancelled or rest on the book; the liquidation residue is
// always discarded).
func (k Keeper) trySingleFill(
	ctx context.Context,
	taker *orderbooktypes.Order,
	isPerp bool,
	now int64,
	apply applyFn,
) (fillResult, error) {
	best, ok, err := k.bookKeeper.PeekBestOpposite(ctx, taker.MarketIndex, taker.IsAsk)
	if err != nil {
		return fillResult{}, err
	}
	if !ok {
		return fillResult{step: fillStepBreak}, nil
	}
	// MarketOrder has no limit price (Price==0); the price-reachable
	// check only applies to limit-style orders. This prevents a buy
	// MarketOrder (or a triggered STOP/TAKE that became a buy
	// MarketOrder) from being self-cancelled by 0 < best.Price.
	if taker.OrderType != perptypes.MarketOrder {
		if taker.IsAsk && taker.Price > best.Price {
			return fillResult{step: fillStepBreak}, nil
		}
		if !taker.IsAsk && taker.Price < best.Price {
			return fillResult{step: fillStepBreak}, nil
		}
	}
	if best.OwnerAccountIndex == taker.OwnerAccountIndex {
		// Self-trade prevention: cancel taker remainder.
		return fillResult{step: fillStepBreak}, nil
	}
	if best.Expiry > 0 && now >= best.Expiry {
		// EvictMakerOrder removes the entry, marks the maker
		// Order as Cancelled, and clears its client / account-
		// open indexes so the now-gone resting order does not
		// linger as a stale "open" record.
		if _, err := k.bookKeeper.EvictMakerOrder(ctx, best.OrderIndex, perptypes.OrderStatusCancelled); err != nil {
			return fillResult{}, err
		}
		return fillResult{step: fillStepContinue}, nil
	}
	// Reduce-only invariant for the taker: if the taker is reduce-only,
	// the fill must only reduce (never grow) the taker's current
	// position absolute value.
	if isPerp && taker.ReduceOnly {
		pos, err := k.accountKeeper.GetPosition(ctx, taker.OwnerAccountIndex, taker.MarketIndex)
		if err != nil {
			return fillResult{}, err
		}
		if pos.Position.IsZero() ||
			(taker.IsAsk && !pos.Position.IsPositive()) ||
			(!taker.IsAsk && !pos.Position.IsNegative()) {
			return fillResult{step: fillStepBreak}, nil
		}
	}
	// Maker reduce-only direction check. A reduce-only maker that
	// no longer holds an opposite position is invalid and must be
	// evicted; previously the entry was dropped but the Order
	// record + indexes leaked, leaving a phantom "open" order.
	if isPerp && best.ReduceOnly {
		pos, err := k.accountKeeper.GetPosition(ctx, best.OwnerAccountIndex, taker.MarketIndex)
		if err != nil {
			return fillResult{}, err
		}
		if pos.Position.IsZero() ||
			(taker.IsAsk && !pos.Position.IsNegative()) ||
			(!taker.IsAsk && !pos.Position.IsPositive()) {
			if _, err := k.bookKeeper.EvictMakerOrder(ctx, best.OrderIndex, perptypes.OrderStatusCancelled); err != nil {
				return fillResult{}, err
			}
			return fillResult{step: fillStepContinue}, nil
		}
	}
	tradeBase := taker.RemainingBaseAmount
	if tradeBase > best.RemainingBaseAmount {
		tradeBase = best.RemainingBaseAmount
	}
	// Cap reduce-only fills to the taker's current position size so a
	// single trade cannot flip the account to the opposite side.
	if isPerp && taker.ReduceOnly {
		pos, err := k.accountKeeper.GetPosition(ctx, taker.OwnerAccountIndex, taker.MarketIndex)
		if err != nil {
			return fillResult{}, err
		}
		limit := pos.Position.Abs().Uint64()
		if limit == 0 {
			return fillResult{step: fillStepBreak}, nil
		}
		if tradeBase > limit {
			tradeBase = limit
		}
	}
	// Symmetric cap on the maker side: a reduce-only maker may not
	// flip its own position. If the maker's reduce capacity is less
	// than its resting size, only fill up to that capacity and let
	// the remainder stay on the book (or get evicted by a follow-up
	// reduce-only direction check).
	if isPerp && best.ReduceOnly && tradeBase > 0 {
		pos, err := k.accountKeeper.GetPosition(ctx, best.OwnerAccountIndex, taker.MarketIndex)
		if err != nil {
			return fillResult{}, err
		}
		makerLimit := pos.Position.Abs().Uint64()
		if makerLimit == 0 {
			if _, err := k.bookKeeper.EvictMakerOrder(ctx, best.OrderIndex, perptypes.OrderStatusCancelled); err != nil {
				return fillResult{}, err
			}
			return fillResult{step: fillStepContinue}, nil
		}
		if tradeBase > makerLimit {
			tradeBase = makerLimit
		}
	}
	if tradeBase == 0 {
		return fillResult{step: fillStepBreak}, nil
	}

	// Each (maker, fill) iteration runs inside an isolated cache
	// context so a recoverable failure (maker risk regression, maker
	// insufficient balance, ...) discards the partial position /
	// collateral / OI writes from the apply closure and the matching
	// loop can evict the bad maker and try the next price level —
	// Lighter parity with `cancel_maker_order` in matching_engine.rs.
	// A taker-side recoverable failure stops the loop but preserves
	// all prior writeCache fills. Hard errors (funding settle / OI /
	// bank failure) propagate up and revert the entire CreateOrder
	// Msg.
	sdkCtx := sdk.UnwrapSDKContext(ctx)
	cacheCtx, writeCache := sdkCtx.CacheContext()
	applyErr := apply(cacheCtx, best, tradeBase)
	if applyErr == nil {
		// FillMakerOrder also runs inside the cache so a downstream
		// failure leaves the orderbook entry untouched.
		if _, err := k.bookKeeper.FillMakerOrder(cacheCtx, best.OrderIndex, tradeBase); err != nil {
			applyErr = err
		}
	}

	switch {
	case applyErr == nil:
		writeCache()
		return fillResult{step: fillStepFilled, base: tradeBase, price: best.Price}, nil
	case tradetypes.IsRecoverableMakerError(applyErr):
		// Discard cache, evict the bad maker on the OUTER ctx so
		// the next loop iteration won't peek the same resting
		// order, then continue.
		if _, err := k.bookKeeper.EvictMakerOrder(ctx, best.OrderIndex, perptypes.OrderStatusCancelled); err != nil {
			return fillResult{}, err
		}
		sdkCtx.EventManager().EmitEvent(sdk.NewEvent(
			types.EventTypeMakerEvictedBadState,
			sdk.NewAttribute(types.AttributeKeyMarketIndex, strconv.FormatUint(uint64(taker.MarketIndex), 10)),
			sdk.NewAttribute(types.AttributeKeyOrderIndex, strconv.FormatUint(best.OrderIndex, 10)),
			sdk.NewAttribute(types.AttributeKeyReason, applyErr.Error()),
		))
		return fillResult{step: fillStepContinue}, nil
	case tradetypes.IsRecoverableTakerError(applyErr):
		// Discard cache. Previously committed fills (any writeCache
		// calls in earlier loop iterations) are retained; the residue
		// is force-cancelled — the taker just proved it cannot satisfy
		// further fills, so resting it on the book would only re-
		// trigger the same failure for downstream takers. Lighter
		// parity: `cancel_taker_order` pops the taker register
		// regardless of TIF.
		sdkCtx.EventManager().EmitEvent(sdk.NewEvent(
			types.EventTypeTakerAbortedBadState,
			sdk.NewAttribute(types.AttributeKeyMarketIndex, strconv.FormatUint(uint64(taker.MarketIndex), 10)),
			sdk.NewAttribute(types.AttributeKeyReason, applyErr.Error()),
		))
		return fillResult{step: fillStepTakerAbort}, nil
	default:
		// Hard error: discard cache and propagate so the whole Msg
		// reverts atomically.
		return fillResult{}, applyErr
	}
}

// matchOrder runs the 12-step matching loop for a user-driven taker
// order. It returns the resulting filled base amount and a status
// (open/partially_filled/filled/cancelled). The per-iteration kernel
// (peek → validate → cacheCtx → apply → classify) lives in
// `trySingleFill`; the outer loop here owns the user-path state
// machine: residue accounting, fills counter, OrderFill event emit,
// and final status decision.
//
// The 12 steps from 17-matching.md §3 (now distributed across
// trySingleFill's body and this outer loop):
//  1. trigger check (handled before this fn for stop/take orders)
//  2. reduce-only invariant (taker side reducing existing position)
//  3. empty-book (POST_ONLY ok; IOC fail; GTT rest)
//  4. POST_ONLY cross check
//  5. price unreachable (limit_price worse than best opposite)
//  6. self-trade prevention
//  7. maker GTT expiry skip
//  8. maker reduce-only invariant
//  9. trade_base = min(remaining_taker, remaining_maker)
//
// 10. fee accounting (delegated to ApplyPerpsMatching/ApplySpotMatching)
// 11. apply matching via trade keeper
// 12. orderbook update (PartialFill / Remove)
func (k Keeper) matchOrder(ctx context.Context, taker *orderbooktypes.Order, maxFills uint32) (uint64, uint32, error) {
	now := sdk.UnwrapSDKContext(ctx).BlockTime().UnixMilli()
	market, err := k.marketKeeper.GetMarket(ctx, taker.MarketIndex)
	if err != nil {
		return 0, perptypes.OrderStatusCancelled, err
	}
	isPerp := market.MarketType == perptypes.MarketTypePerps

	apply := func(cacheCtx context.Context, best orderbooktypes.OrderBookEntry, tradeBase uint64) error {
		if isPerp {
			return k.tradeKeeper.ApplyPerpsMatching(cacheCtx, tradekeeper.PerpFill{
				MakerAccountIndex: best.OwnerAccountIndex,
				TakerAccountIndex: taker.OwnerAccountIndex,
				MarketIndex:       taker.MarketIndex,
				Price:             best.Price,
				BaseAmount:        tradeBase,
				IsTakerAsk:        taker.IsAsk,
				TakerFee:          market.TakerFee,
				MakerFee:          market.MakerFee,
			})
		}
		return k.tradeKeeper.ApplySpotMatching(cacheCtx, tradekeeper.SpotFill{
			MakerAccountIndex: best.OwnerAccountIndex,
			TakerAccountIndex: taker.OwnerAccountIndex,
			MarketIndex:       taker.MarketIndex,
			Price:             best.Price,
			BaseAmount:        tradeBase,
			IsTakerAsk:        taker.IsAsk,
			TakerFee:          market.TakerFee,
			MakerFee:          market.MakerFee,
		}, market.BaseAssetId, market.QuoteAssetId)
	}

	var totalFilled uint64
	var fills uint32
	for taker.RemainingBaseAmount > 0 {
		if fills >= maxFills {
			return totalFilled, perptypes.OrderStatusPartiallyFilled, nil
		}
		res, err := k.trySingleFill(ctx, taker, isPerp, now, apply)
		if err != nil {
			return totalFilled, perptypes.OrderStatusCancelled, err
		}
		switch res.step {
		case fillStepBreak:
			goto done
		case fillStepContinue:
			continue
		case fillStepTakerAbort:
			return totalFilled, perptypes.OrderStatusCancelled, nil
		case fillStepFilled:
			taker.RemainingBaseAmount -= res.base
			totalFilled += res.base
			fills++
			sdk.UnwrapSDKContext(ctx).EventManager().EmitEvent(sdk.NewEvent(
				types.EventTypeOrderFill,
				sdk.NewAttribute(types.AttributeKeyMarketIndex, strconv.FormatUint(uint64(taker.MarketIndex), 10)),
				sdk.NewAttribute(types.AttributeKeyPrice, strconv.FormatUint(uint64(res.price), 10)),
				sdk.NewAttribute(types.AttributeKeyBase, strconv.FormatUint(res.base, 10)),
			))
		}
	}
done:
	if taker.RemainingBaseAmount == 0 {
		return totalFilled, perptypes.OrderStatusFilled, nil
	}
	if totalFilled > 0 {
		return totalFilled, perptypes.OrderStatusPartiallyFilled, nil
	}
	return totalFilled, perptypes.OrderStatusOpen, nil
}

// matchLiquidationLoop runs the matching loop for a system-issued
// `LIQUIDATION_ORDER + IOC + reduce_only` taker. The synthetic taker
// is owned by the victim and is NEVER persisted to the orderbook —
// IOC residue is silently discarded by the caller (`MatchLiquidationOrder`).
//
// Invariants the caller is expected to satisfy:
//
//   - taker.OrderType == LiquidationOrder
//   - taker.TimeInForce == IOC
//   - taker.ReduceOnly == true
//   - taker.OwnerAccountIndex == victim
//   - taker.Price == zeroPrice (zero-price floor; the price-reachable
//     check inside trySingleFill guarantees fills only happen at maker
//     prices not worse than zeroPrice, matching Lighter's "fill at or
//     better than zero price" guarantee)
//
// Per-fill PerpFill differences vs the user path:
//
//   - TakerFee / MakerFee are 0 (the only fee the close-out pays is
//     the liquidation improvement fee).
//   - ZeroPrice / LiquidationFeeBps / LiquidationFeeRecipient flow into
//     the trade engine so improvement above the zero-price floor is
//     captured and routed to the LLP / Insurance Fund.
//   - SkipMakerRiskCheck is set: the maker IS the victim — the fill
//     mechanically improves their TAV/MMR ratio, but IsValidRiskChange
//     would still reject because post is not HEALTHY.
//
// After every successful fill the loop re-evaluates the victim's
// health (cross or per-market isolated, matching the liquidation
// keeper's `victimHealthForPosition` rule) and breaks early when the
// account is no longer in PARTIAL/FULL liquidation. This is Lighter's
// `is_not_in_liquidation_and_is_liquidation_order` short-circuit: a
// liquidation order keeps consuming the book only as long as the
// victim still needs deleveraging.
func (k Keeper) matchLiquidationLoop(
	ctx context.Context,
	taker *orderbooktypes.Order,
	maxFills uint32,
	zeroPrice uint32,
	liquidationFeeBps uint32,
	liquidationFeeRecipient uint64,
) (uint64, uint32, error) {
	now := sdk.UnwrapSDKContext(ctx).BlockTime().UnixMilli()
	market, err := k.marketKeeper.GetMarket(ctx, taker.MarketIndex)
	if err != nil {
		return 0, perptypes.OrderStatusCancelled, err
	}
	// Liquidation orders only exist for perps markets in Lighter's
	// design. Spot markets have no notion of liquidation.
	if market.MarketType != perptypes.MarketTypePerps {
		return 0, perptypes.OrderStatusCancelled, types.ErrInvalidOrder.Wrapf(
			"liquidation order requires perps market (got type=%d)", market.MarketType,
		)
	}

	apply := func(cacheCtx context.Context, best orderbooktypes.OrderBookEntry, tradeBase uint64) error {
		return k.tradeKeeper.ApplyPerpsMatching(cacheCtx, tradekeeper.PerpFill{
			MakerAccountIndex:       best.OwnerAccountIndex,
			TakerAccountIndex:       taker.OwnerAccountIndex,
			MarketIndex:             taker.MarketIndex,
			Price:                   best.Price,
			BaseAmount:              tradeBase,
			IsTakerAsk:              taker.IsAsk,
			TakerFee:                0,
			MakerFee:                0,
			ZeroPrice:               zeroPrice,
			LiquidationFeeBps:       liquidationFeeBps,
			LiquidationFeeRecipient: liquidationFeeRecipient,
			// Victim is the taker here (the synthetic order is
			// owned by the victim). The "victim is the maker"
			// SkipMakerRiskCheck convention from x/trade is for
			// the legacy direct-PerpFill liquidation path; for the
			// orderbook IOC path the victim's risk regression is
			// expected on the taker side until the loop's health
			// short-circuit fires. We therefore use NoRiskCheck on
			// both sides: the maker on the public book is a normal
			// account whose post-trade health was vetted when its
			// resting order was placed, and the taker (victim) is
			// being closed-out by construction.
			NoRiskCheck: true,
		})
	}

	var totalFilled uint64
	var fills uint32
	for taker.RemainingBaseAmount > 0 {
		if fills >= maxFills {
			return totalFilled, perptypes.OrderStatusPartiallyFilled, nil
		}
		res, err := k.trySingleFill(ctx, taker, true /*isPerp*/, now, apply)
		if err != nil {
			return totalFilled, perptypes.OrderStatusCancelled, err
		}
		switch res.step {
		case fillStepBreak:
			goto done
		case fillStepContinue:
			continue
		case fillStepTakerAbort:
			return totalFilled, perptypes.OrderStatusCancelled, nil
		case fillStepFilled:
			taker.RemainingBaseAmount -= res.base
			totalFilled += res.base
			fills++
			sdk.UnwrapSDKContext(ctx).EventManager().EmitEvent(sdk.NewEvent(
				types.EventTypeOrderFill,
				sdk.NewAttribute(types.AttributeKeyMarketIndex, strconv.FormatUint(uint64(taker.MarketIndex), 10)),
				sdk.NewAttribute(types.AttributeKeyPrice, strconv.FormatUint(uint64(res.price), 10)),
				sdk.NewAttribute(types.AttributeKeyBase, strconv.FormatUint(res.base, 10)),
			))
			// Health short-circuit: stop consuming the book the
			// moment the victim is no longer in liquidation. This
			// is intentionally placed AFTER writeCache so the
			// just-applied fill commits even if the post-fill
			// account becomes healthy on the same iteration.
			stillUnder, err := k.isStillUnderLiquidation(ctx, taker.OwnerAccountIndex, taker.MarketIndex)
			if err != nil {
				return totalFilled, perptypes.OrderStatusCancelled, err
			}
			if !stillUnder {
				goto done
			}
		}
	}
done:
	if taker.RemainingBaseAmount == 0 {
		return totalFilled, perptypes.OrderStatusFilled, nil
	}
	if totalFilled > 0 {
		return totalFilled, perptypes.OrderStatusPartiallyFilled, nil
	}
	return totalFilled, perptypes.OrderStatusCancelled, nil
}

// isStillUnderLiquidation reports whether a victim is still subject
// to (partial or full) liquidation in the targeted market. The
// classification mirrors x/liquidation's `victimHealthForPosition`:
// cross-mode positions consult the cross account health; isolated
// positions consult the per-market isolated health, since each
// isolated position is its own risk envelope.
//
// Used exclusively by `matchLiquidationLoop` to implement the Lighter
// `is_not_in_liquidation_and_is_liquidation_order` short-circuit. It
// is intentionally NOT called from `matchOrder` so the user-path
// matching loop pays no per-fill risk-keeper read.
func (k Keeper) isStillUnderLiquidation(ctx context.Context, victim uint64, marketIdx uint32) (bool, error) {
	pos, err := k.accountKeeper.GetPosition(ctx, victim, marketIdx)
	if err != nil {
		return false, err
	}
	var s uint32
	if pos.MarginMode == perptypes.IsolatedMargin {
		s, err = k.riskKeeper.GetIsolatedHealthStatus(ctx, victim, marketIdx)
	} else {
		s, err = k.riskKeeper.GetHealthStatus(ctx, victim)
	}
	if err != nil {
		return false, err
	}
	return s == perptypes.HealthPartialLiquidation || s == perptypes.HealthFullLiquidation, nil
}
