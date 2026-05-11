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
// the `realized_pnl → fee → margin_delta → risk` sequence on the
// right account / market without re-loading state.
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
// `ENTRY_QUOTE_BITS` (`is_new_position_valid` failure mode).
// `Apply` re-wraps it into `ErrMakerInvalidPosition` /
// `ErrTakerInvalidPosition` so the matching loop can route the failure
// through `IsRecoverable*Error`.
var errPositionOutOfBounds = errors.New("trade: post-trade position out of bounds")

// applyPositionChange handles the four position-change scenarios from
// 14-trade.md §3.2: open new, increase, decrease, flip. It computes the
// new position size + entry_quote and the realized PnL but does NOT
// route the realized PnL anywhere — `applyPositionFinancials` does
// that based on the position's margin mode.
//
// The four-quadrant arithmetic itself lives in
// `accounttypes.AccountPosition.ApplyFill`, shared with x/risk's
// `SimulateRiskAfterTakeover`. This wrapper is responsible only for
// driving the persisted RMW + bounds-check + OI delta around it.
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
		sideFlipped bool
	)

	updated, err := e.accountKeeper.UpdatePosition(ctx, accountIdx, marketIdx, func(pos *accounttypes.AccountPosition) error {
		// `pos` is already nil-normalised by GetPosition (the RMW
		// helper auto-vivifies a zero-valued record). Capture the
		// pre-state by value so res.Old reflects the position as it
		// was before this fill.
		old = *pos
		curSize = pos.BaseSize
		delta := math.NewIntFromUint64(baseAmount).MulRaw(sign)

		fill := pos.ApplyFill(delta, price)
		// ApplyFill is a pure function; we still need to mirror its
		// new size / entry_quote into the persisted record before
		// setPosition runs.
		pos.BaseSize = fill.Position.BaseSize
		pos.EntryQuote = fill.Position.EntryQuote
		newSize = pos.BaseSize
		realizedPnL = fill.RealizedPnL
		sideFlipped = fill.SideFlipped

		// Bounds check ahead of persistence so we never store a
		// position the prover circuit would reject.
		if !isWithinPositionBounds(pos.BaseSize, pos.EntryQuote) {
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
		New:         updated,
		OIDelta:     oiDelta.Int64(),
		SideFlipped: sideFlipped,
		RealizedPnL: realizedPnL,
	}, nil
}

// isWithinPositionBounds enforces the prover circuit's hard limits
// `|position| < 2^POSITION_SIZE_BITS` and `|entry_quote| < 2^ENTRY_QUOTE_BITS`.
// `position.is_valid` checks the same envelope.
func isWithinPositionBounds(position, entryQuote math.Int) bool {
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
