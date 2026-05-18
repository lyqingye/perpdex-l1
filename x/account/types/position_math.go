package types

import (
	"cosmossdk.io/math"

	perptypes "github.com/perpdex/perpdex-l1/types"
	markettypes "github.com/perpdex/perpdex-l1/x/market/types"
)

func (p *AccountPosition) NormalizeIntFields() {
	if p.BaseSize.IsNil() {
		p.BaseSize = math.ZeroInt()
	}
	if p.EntryQuote.IsNil() {
		p.EntryQuote = math.ZeroInt()
	}
	if p.LastFundingRatePrefixSum.IsNil() {
		p.LastFundingRatePrefixSum = math.ZeroInt()
	}
	if p.AllocatedMargin.IsNil() {
		p.AllocatedMargin = math.ZeroInt()
	}
}

// IsSameSide returns true iff `a` and `b` are both non-zero and share the
// same sign. The perp domain uses it to classify whether base size and
// `Delta` point the same way (increase vs decrease vs flip), so a zero
// input returns false rather than true — "no direction" is not "same
// direction".
func IsSameSide(a, b math.Int) bool {
	if a.IsZero() || b.IsZero() {
		return false
	}
	return a.IsNegative() == b.IsNegative()
}

// IsLong reports whether the position is long. Zero base size is treated as long.
func (p AccountPosition) IsLong() bool {
	return !p.BaseSize.IsNegative()
}

// IsShort reports whether the position is short. Zero base size is not short.
func (p AccountPosition) IsShort() bool {
	return p.BaseSize.IsNegative()
}

// OpeningIsBid reports whether opening this position uses the bid side. Zero
// base size is treated as bid.
func (p AccountPosition) OpeningIsBid() bool {
	return p.IsLong()
}

// OpeningIsAsk reports whether opening this position uses the ask side. Zero
// base size is not ask.
func (p AccountPosition) OpeningIsAsk() bool {
	return p.IsShort()
}

// Notional returns |base_size| * mark. Returns ZeroInt when either the base
// size is zero or `markPrice` is zero. Pure value receiver, no I/O.
func (p AccountPosition) Notional(markPrice uint32) math.Int {
	if p.BaseSize.IsZero() || markPrice == 0 {
		return math.ZeroInt()
	}
	return p.BaseSize.Abs().Mul(math.NewIntFromUint64(uint64(markPrice)))
}

// MarginRequirement returns Notional * fractionBps / MarginTick. `fractionBps`
// is one of MarketDetails.{DefaultInitialMarginFraction, MaintenanceMarginFraction,
// CloseOutMarginFraction}; the helper exists so callers can plug in any of
// the three without re-deriving the multiplication.
//
// Callers are responsible for ensuring `fractionBps <= MarginTick`. That
// invariant is enforced by x/market's parameter validation, not re-clamped
// here.
func (p AccountPosition) MarginRequirement(markPrice uint32, fractionBps uint32) math.Int {
	if fractionBps == 0 {
		return math.ZeroInt()
	}
	notional := p.Notional(markPrice)
	if notional.IsZero() {
		return math.ZeroInt()
	}
	return notional.Mul(math.NewIntFromUint64(uint64(fractionBps))).
		Quo(math.NewInt(int64(perptypes.MarginTick)))
}

// InitialMargin returns the position's IM at `markPrice` using the market's
// `default_initial_margin_fraction`. Only meaningful for perp markets; spot
// markets are settled by the spot keeper and never reach here.
func (p AccountPosition) InitialMargin(markPrice uint32, md markettypes.MarketDetails) math.Int {
	return md.InitialMargin(p.BaseSize.Abs(), markPrice)
}

func (p AccountPosition) MaintenanceMargin(markPrice uint32, md markettypes.MarketDetails) math.Int {
	return p.MarginRequirement(markPrice, md.MaintenanceMarginFraction)
}

func (p AccountPosition) CloseOutMargin(markPrice uint32, md markettypes.MarketDetails) math.Int {
	return p.MarginRequirement(markPrice, md.CloseOutMarginFraction)
}

// UnrealizedPnL returns base_size * mark - entry_quote at `markPrice`. Sign is
// positive when the position is in profit. Returns ZeroInt for empty positions
// or when the mark is zero.
func (p AccountPosition) UnrealizedPnL(markPrice uint32) math.Int {
	if p.BaseSize.IsZero() || markPrice == 0 {
		return math.ZeroInt()
	}
	return p.BaseSize.Mul(math.NewIntFromUint64(uint64(markPrice))).Sub(p.EntryQuote)
}

func (p AccountPosition) MarketValue(markPrice uint32) math.Int {
	allocated := p.AllocatedMargin
	if allocated.IsNil() {
		allocated = math.ZeroInt()
	}
	return allocated.Add(p.UnrealizedPnL(markPrice))
}

// FillResult describes the post-trade snapshot produced by ApplyFill. Pure
// value type:
//
//   - Position is the post-trade position (updated BaseSize and
//     EntryQuote); all other fields (AccountIndex / MarketIndex /
//     MarginMode / AllocatedMargin / …) are carried over from the input.
//   - RealizedPnL is the PnL realised by this fill; non-zero only in the
//     decrease | close | flip scenarios.
//   - SideFlipped is true iff the position crossed zero and reversed
//     direction, signalling callers to re-evaluate IM on the residual leg.
//
// FillResult is the **math layer** output (pure value, no I/O). The
// **keeper layer** wraps this with persistence + event emission +
// position_id allocation into FillApplyResult (returned by
// Keeper.ApplyFill); external callers (x/trade) consume
// FillApplyResult exclusively and never need to touch the math layer
// or the package-private lifecycle primitives directly. Persistence
// and bounds checks (POSITION_SIZE_BITS / ENTRY_QUOTE_BITS) live on
// the keeper layer.
type FillResult struct {
	Position    AccountPosition
	RealizedPnL math.Int
	SideFlipped bool
}

// FillApplyResult is the keeper-level output of Keeper.ApplyFill: the
// **cohesive** fill-application entrypoint that owns the entire
// transition (open / mutate / close / flip), persistence, and event
// emission for one side of one fill. Callers (x/trade.Engine.Apply)
// receive the post-state needed for downstream pipelines (fee charge,
// isolated reconciliation, OI delta, post-trade risk check) without
// ever issuing a position-keeper RMW closure of their own.
//
// Field semantics:
//
//   - Old: the position snapshot **before** the fill. Used by
//     downstream isolated rebalance (`calculate_isolated_margin_change`)
//     which needs both the old and new uPnL.
//
//   - New: the position snapshot **after** the fill. On Closed, this
//     carries the **pre-close** values (BaseSize / AllocatedMargin /
//     EntryQuote etc. as they were the moment before the close) so
//     the isolated close branch can drain residual fields back to
//     cross collateral; `New.BaseSize.IsZero()` is NOT a reliable
//     closed indicator — use the explicit Closed bool instead.
//
//   - RealizedPnL: the PnL realised by this fill (decrease / close /
//     flip closing leg). Routed by the engine into the right margin
//     pool (cross collateral or isolated allocated_margin).
//
//   - SideFlipped: pre/post BaseSize have opposite signs (the fill
//     crossed zero). The keeper emits Closed (old id) + Opened (new
//     id) atomically; the engine uses this to know the new lifeline
//     started with a fresh position_id even though the BaseSize stayed
//     non-zero throughout.
//
//   - Closed: pre.BaseSize != 0 AND post.BaseSize == 0 (fully closed,
//     not a flip). The engine's close branch keys on this to skip
//     allocated_margin writes (the row may be gone) and route refunds
//     straight to cross collateral.
//
//   - OIDelta: |new.BaseSize| - |old.BaseSize|. Sum across maker /
//     taker divided by 2 yields the per-fill open-interest delta.
type FillApplyResult struct {
	Old         AccountPosition
	New         AccountPosition
	RealizedPnL math.Int
	SideFlipped bool
	Closed      bool
	OIDelta     int64
}

// ApplyFill projects the post-trade state of this position after a single
// fill of `delta` (signed base amount; positive = buy, negative = sell) at
// integer `price`. Pure math implementing `apply_match_order` and covering
// the four canonical scenarios:
//
//  1. open new        : curSize == 0
//     ⇒ entry_quote = delta * price
//
//  2. increase        : sign(curSize) == sign(delta)
//     ⇒ entry_quote += delta * price
//
//  3. decrease | close: opposite sign, |delta| <= |curSize|
//     ⇒ entry_quote scaled to remaining size, realized_pnl realized
//     for the closed portion via
//     `realized_pnl = trade_quote + curEntryQuote * delta / -curSize`.
//
//  4. flip            : opposite sign, |delta| > |curSize|
//     ⇒ realized_pnl realizes the closing leg, entry_quote reset to
//     residual leg's notional, side_flipped = true.
//
// Both x/account `Keeper.ApplyFill` (which persists the result via
// the package-private open / mutate / close lifecycle primitives) and
// x/risk `SimulateRiskAfterTakeover` (which inspects the post-state
// for IM/MM/CM aggregation) consume this as the single source of
// truth for fill-side classification.
func (p AccountPosition) ApplyFill(delta math.Int, price uint32) FillResult {
	curSize := p.BaseSize
	curEntryQuote := p.EntryQuote

	priceInt := math.NewIntFromUint64(uint64(price))
	// notional carries the trade-direction sign by construction:
	// notional = delta * price = (sign * baseAmount) * price.
	notional := delta.Mul(priceInt)
	newSize := curSize.Add(delta)

	var newEntryQuote math.Int
	realizedPnL := math.ZeroInt()

	switch {
	case curSize.IsZero():
		newEntryQuote = notional

	case IsSameSide(curSize, delta):
		newEntryQuote = curEntryQuote.Add(notional)

	case newSize.IsZero() || IsSameSide(curSize, newSize):
		// pure decrease (or full close): realize partial PnL,
		// scale entry_quote proportionally to remaining size.
		realizedPnL = notional.Add(curEntryQuote.Mul(delta).Quo(curSize.Neg()))
		if newSize.IsZero() {
			newEntryQuote = math.ZeroInt()
		} else {
			newEntryQuote = curEntryQuote.Mul(newSize).Quo(curSize)
		}

	default:
		// flip: close existing then open in opposite direction. The
		// trade-side notional for the closing portion is
		// `closeBase * price * sign(trade)`. Using `sign(delta)` keeps
		// closeNotional and curEntryQuote on opposite-equal sides so
		// realizedPnL captures only the actual PnL — using
		// `sign(curSize)` instead would inflate realizedPnL by 2x the
		// closing leg's notional.
		closeBase := curSize.Abs()
		closeNotional := closeBase.Mul(priceInt)
		if delta.IsNegative() {
			closeNotional = closeNotional.Neg()
		}
		realizedPnL = closeNotional.Add(curEntryQuote)
		// residual same sign as delta; residualNotional unsigned-multiplied.
		residual := newSize
		newEntryQuote = residual.Mul(priceInt)
	}

	next := p
	next.BaseSize = newSize
	next.EntryQuote = newEntryQuote

	sideFlipped := !curSize.IsZero() && !newSize.IsZero() && !IsSameSide(curSize, newSize)

	return FillResult{
		Position:    next,
		RealizedPnL: realizedPnL,
		SideFlipped: sideFlipped,
	}
}
