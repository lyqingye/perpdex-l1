package types

import (
	"cosmossdk.io/math"

	perptypes "github.com/perpdex/perpdex-l1/types"
	markettypes "github.com/perpdex/perpdex-l1/x/market/types"
)

// IsSameSide returns true iff `a` and `b` are both non-zero and share the
// same sign. perp 域用来判断 Position 与 Delta 是否同向（增仓 vs 减仓 vs 翻仓）；
// 因此 zero 输入返回 false 而不是 true（"无方向" 不算同向）。
//
// Centralised here to retire the duplicate sameSign / sameSignInt helpers
// that previously lived inside x/trade/keeper/perp and x/risk/keeper.
func IsSameSide(a, b math.Int) bool {
	if a.IsZero() || b.IsZero() {
		return false
	}
	return a.IsNegative() == b.IsNegative()
}

// Notional returns |position| * mark. Returns ZeroInt when either the
// position size is nil/zero or `markPrice` is zero. Pure value receiver,
// no I/O.
func (p AccountPosition) Notional(markPrice uint32) math.Int {
	if p.Position.IsNil() || p.Position.IsZero() || markPrice == 0 {
		return math.ZeroInt()
	}
	return p.Position.Abs().Mul(math.NewIntFromUint64(uint64(markPrice)))
}

// MarginRequirement returns Notional * fractionBps / MarginTick. `fractionBps`
// is one of MarketDetails.{DefaultInitialMarginFraction, MaintenanceMarginFraction,
// CloseOutMarginFraction}; the helper exists so callers can plug in any of
// the three without re-deriving the multiplication.
//
// 调用方负责保证 fractionBps <= MarginTick；不超过该上限是 x/market 的入参检查
// 责任，本函数不再 clamp。
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
// `default_initial_margin_fraction`. 仅对 perp 市场有意义；spot market 应直接
// 走 spot keeper，不会调到这里。
func (p AccountPosition) InitialMargin(markPrice uint32, md markettypes.MarketDetails) math.Int {
	return p.MarginRequirement(markPrice, md.DefaultInitialMarginFraction)
}

// MaintenanceMargin returns the position's MM at `markPrice` using the market's
// `maintenance_margin_fraction`.
func (p AccountPosition) MaintenanceMargin(markPrice uint32, md markettypes.MarketDetails) math.Int {
	return p.MarginRequirement(markPrice, md.MaintenanceMarginFraction)
}

// CloseOutMargin returns the position's CM at `markPrice` using the market's
// `close_out_margin_fraction`.
func (p AccountPosition) CloseOutMargin(markPrice uint32, md markettypes.MarketDetails) math.Int {
	return p.MarginRequirement(markPrice, md.CloseOutMarginFraction)
}

// UnrealizedPnL returns position * mark - entry_quote at `markPrice`. Sign is
// positive when the position is in profit. Returns ZeroInt for empty positions
// or when the mark is zero.
func (p AccountPosition) UnrealizedPnL(markPrice uint32) math.Int {
	if p.Position.IsNil() || p.Position.IsZero() || markPrice == 0 {
		return math.ZeroInt()
	}
	entryQuote := p.EntryQuote
	if entryQuote.IsNil() {
		entryQuote = math.ZeroInt()
	}
	return p.Position.Mul(math.NewIntFromUint64(uint64(markPrice))).Sub(entryQuote)
}

// FillResult describes the post-trade snapshot produced by ApplyFill. Pure
// value type:
//
//   - Position 是成交后的新仓位（含 Position size 与 EntryQuote），其它字段
//     从原仓位透传（AccountIndex / MarketIndex / MarginMode / AllocatedMargin / …）。
//   - RealizedPnL 是本次成交实现的 PnL；非零仅出现在 decrease|close|flip 场景。
//   - SideFlipped 当 Position 穿越零点反向时为 true（用于上层路由 IM 重算）。
//
// 调用方自行负责持久化（SetPosition / UpdatePosition）和 bounds 检查
// （POSITION_SIZE_BITS / ENTRY_QUOTE_BITS）。
type FillResult struct {
	Position    AccountPosition
	RealizedPnL math.Int
	SideFlipped bool
}

// ApplyFill projects the post-trade state of this position after a single
// fill of `delta` (signed base amount; positive = buy, negative = sell) at
// integer `price`. Pure math; mirrors lighter `apply_match_order` and
// covers the four canonical scenarios:
//
//  1. open new        : curSize == 0
//     ⇒ entry_quote = delta * price
//
//  2. increase        : sign(curSize) == sign(delta)
//     ⇒ entry_quote += delta * price
//
//  3. decrease | close: opposite sign, |delta| <= |curSize|
//     ⇒ entry_quote scaled to remaining size, realized_pnl realized
//     for the closed portion. Mirrors lighter
//     `realized_pnl = trade_quote + curEntryQuote * delta / -curSize`.
//
//  4. flip            : opposite sign, |delta| > |curSize|
//     ⇒ realized_pnl realizes the closing leg, entry_quote reset to
//     residual leg's notional, side_flipped = true.
//
// Both x/trade `applyPositionChange` (which persists the result via
// UpdatePosition) and x/risk `SimulateRiskAfterTakeover` (which inspects
// the post-state for IM/MM/CM aggregation) consume this single source of
// truth, retiring the duplicate switch that previously lived in both.
func (p AccountPosition) ApplyFill(delta math.Int, price uint32) FillResult {
	curSize := p.Position
	if curSize.IsNil() {
		curSize = math.ZeroInt()
	}
	curEntryQuote := p.EntryQuote
	if curEntryQuote.IsNil() {
		curEntryQuote = math.ZeroInt()
	}
	if delta.IsNil() {
		delta = math.ZeroInt()
	}

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
	next.Position = newSize
	next.EntryQuote = newEntryQuote

	sideFlipped := !curSize.IsZero() && !newSize.IsZero() && !IsSameSide(curSize, newSize)

	return FillResult{
		Position:    next,
		RealizedPnL: realizedPnL,
		SideFlipped: sideFlipped,
	}
}
