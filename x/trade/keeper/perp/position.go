package perp

import (
	"context"
	"errors"

	"cosmossdk.io/math"

	perptypes "github.com/perpdex/perpdex-l1/types"
	accounttypes "github.com/perpdex/perpdex-l1/x/account/types"
)

// positionChangeResult bundles the inputs / outputs of one side's
// position update so the surrounding Apply pipeline can chain through
// the lighter `realized_pnl → fee → margin_delta → risk` sequence on
// the right account / market without re-loading state.
//
// `New` reflects the position AFTER size + entry_quote are written,
// but BEFORE the realized-PnL / fee / margin_delta routing — the
// helpers below mutate `New.AllocatedMargin` as they fold those flows
// in (and re-persist via `SetPosition` whenever necessary).
type positionChangeResult struct {
	AccountIdx  uint64
	MarketIdx   uint32
	Old         accounttypes.AccountPosition
	New         accounttypes.AccountPosition
	OIDelta     int64
	SideFlipped bool
	RealizedPnL math.Int
}

// errPositionOutOfBounds is the internal sentinel returned by
// `applyPositionChange` when the post-trade `|position|` or
// `|entry_quote|` would overflow `POSITION_SIZE_BITS` /
// `ENTRY_QUOTE_BITS` (lighter `is_new_position_valid` failure mode).
// `Apply` re-wraps it into `ErrMakerInvalidPosition` /
// `ErrTakerInvalidPosition` so the matching loop can route the failure
// through `IsRecoverable*Error`.
var errPositionOutOfBounds = errors.New("trade: post-trade position out of bounds")

// applyPositionChange handles the four position-change scenarios from
// 14-trade.md §3.2: open new, increase, decrease, flip. It computes the
// new position size + entry_quote and the realized PnL but does NOT
// route the realized PnL anywhere — `applyPositionFinancials` does
// that based on the position's margin mode (lighter parity).
//
// The returned `positionChangeResult` carries enough context for the
// caller to drive the rest of the lighter `apply_perps_trade` pipeline
// (fee routing, isolated margin auto-allocation, risk check).
//
// `errPositionOutOfBounds` is returned when the new size or entry
// quote would overflow the bit-width bounds enforced by the prover
// circuit; the caller wraps it into the appropriate maker / taker
// sentinel.
func (e Engine) applyPositionChange(ctx context.Context, accountIdx uint64, marketIdx uint32, price uint32, baseAmount uint64, sign int64) (positionChangeResult, error) {
	var (
		old         accounttypes.AccountPosition
		curSize     math.Int
		newSize     math.Int
		realizedPnL = math.ZeroInt()
	)

	updated, err := e.accountKeeper.UpdatePosition(ctx, accountIdx, marketIdx, func(pos *accounttypes.AccountPosition) error {
		old = clonePosition(*pos)
		curSize = pos.Position
		delta := math.NewIntFromUint64(baseAmount).MulRaw(sign)
		newSize = curSize.Add(delta)

		curEntryQuote := pos.EntryQuote
		if curEntryQuote.IsNil() {
			curEntryQuote = math.ZeroInt()
		}
		notional := math.NewIntFromUint64(baseAmount).Mul(math.NewIntFromUint64(uint64(price))).MulRaw(sign)

		switch {
		case curSize.IsZero():
			// open new position
			pos.EntryQuote = notional
		case sameSign(curSize, delta):
			// increase
			pos.EntryQuote = curEntryQuote.Add(notional)
		case newSize.IsZero() || sameSign(curSize, newSize):
			// pure decrease (or close): realize partial PnL
			realizedPnL = notional.Add(curEntryQuote.Mul(delta).Quo(curSize.Neg()))
			// scale entry_quote proportionally to remaining size
			if curSize.IsZero() {
				pos.EntryQuote = math.ZeroInt()
			} else {
				pos.EntryQuote = curEntryQuote.Mul(newSize).Quo(curSize)
			}
		default:
			// flip: close existing then open in opposite direction.
			//
			// Of the `baseAmount` units traded, `|curSize|` units close
			// the existing position and the remainder opens the new one
			// on the opposite side. The trade-side notional for the
			// closing portion is `closeBase * price * sign` (signed by
			// the trade direction, NOT the position direction): if the
			// trade is a sell, the closing leg also sells, so the
			// notional carries `sign = -1`. Using `-sign` here would
			// produce a +/- mismatch between `closeNotional` and
			// `curEntryQuote` and inflate `realized_pnl` by 2× the
			// closing leg's notional — corrupting PnL realization on
			// every flip.
			closeBase := curSize.Abs()
			closeNotional := closeBase.Mul(math.NewIntFromUint64(uint64(price))).MulRaw(sign)
			realizedPnL = closeNotional.Add(curEntryQuote)
			residual := delta.Add(curSize) // residual same sign as delta
			residualNotional := residual.Mul(math.NewIntFromUint64(uint64(price)))
			pos.EntryQuote = residualNotional
		}
		pos.Position = newSize

		// Bounds check ahead of persistence so we never store a
		// position the prover circuit would reject.
		if !isWithinPositionBounds(pos.Position, pos.EntryQuote) {
			return errPositionOutOfBounds
		}
		return nil
	})
	if err != nil {
		return positionChangeResult{}, err
	}

	// OI contribution from this account: |new| - |old|. Positive when the
	// account grows its exposure, negative when reducing / closing.
	oiDelta := newSize.Abs().Sub(curSize.Abs())
	return positionChangeResult{
		AccountIdx:  accountIdx,
		MarketIdx:   marketIdx,
		Old:         old,
		New:         clonePosition(updated),
		OIDelta:     oiDelta.Int64(),
		SideFlipped: !curSize.IsZero() && !newSize.IsZero() && !sameSign(curSize, newSize),
		RealizedPnL: realizedPnL,
	}, nil
}

// isWithinPositionBounds enforces the prover circuit's hard limits
// `|position| < 2^POSITION_SIZE_BITS` and `|entry_quote| < 2^ENTRY_QUOTE_BITS`.
// Lighter `position.is_valid` checks the same envelope.
func isWithinPositionBounds(position, entryQuote math.Int) bool {
	if position.IsNil() {
		position = math.ZeroInt()
	}
	if entryQuote.IsNil() {
		entryQuote = math.ZeroInt()
	}
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
