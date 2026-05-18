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
// Persistence (the caller's choice between OpenPosition /
// MutatePosition / ClosePosition on the account keeper) and bounds
// checks (POSITION_SIZE_BITS / ENTRY_QUOTE_BITS) remain the caller's
// responsibility.
type FillResult struct {
	Position    AccountPosition
	RealizedPnL math.Int
	SideFlipped bool
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
// Both x/trade `applyPositionChange` (which persists the result via
// the open / mutate / close lifecycle methods) and x/risk
// `SimulateRiskAfterTakeover` (which inspects the post-state for
// IM/MM/CM aggregation) consume this as the single source of truth
// for fill-side classification.
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
