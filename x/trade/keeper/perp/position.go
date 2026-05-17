package perp

import (
	"context"
	"errors"

	"cosmossdk.io/math"

	perptypes "github.com/perpdex/perpdex-l1/types"
	accounttypes "github.com/perpdex/perpdex-l1/x/account/types"
)

// positionChangeResult bundles one side's position update so the
// Apply pipeline can chain realized_pnl → fee → margin_delta → risk
// without re-loading state. New reflects post-size/entry_quote but
// pre-routing; downstream helpers mutate New.AllocatedMargin as they
// fold subsequent flows in.
type positionChangeResult struct {
	AccountIdx  uint64
	MarketIdx   uint32
	Old         accounttypes.AccountPosition
	New         accounttypes.AccountPosition
	OIDelta     int64
	SideFlipped bool
	RealizedPnL math.Int
}

// errPositionOutOfBounds signals that |position| or |entry_quote|
// would overflow POSITION_SIZE_BITS / ENTRY_QUOTE_BITS. Apply
// re-wraps it into Maker/Taker InvalidPosition for the matching loop.
var errPositionOutOfBounds = errors.New("trade: post-trade position out of bounds")

// applyPositionChange computes the new size / entry_quote / realized
// PnL for one side (open / increase / decrease / flip). Does NOT
// route the PnL — that is the margin-mode dispatcher's job. The
// quadrant math itself lives in AccountPosition.ApplyFill (shared
// with risk.SimulateRiskAfterTakeover); this wrapper drives the
// persisted RMW, bounds check, and OI delta.
func (e Engine) applyPositionChange(ctx context.Context, accountIdx uint64, marketIdx uint32, price uint32, baseAmount uint64, sign int64) (positionChangeResult, error) {
	var (
		old         accounttypes.AccountPosition
		curSize     math.Int
		newSize     math.Int
		realizedPnL = math.ZeroInt()
		sideFlipped bool
	)

	updated, err := e.accountKeeper.UpdatePosition(ctx, accountIdx, marketIdx, func(pos *accounttypes.AccountPosition) error {
		// Capture pre-state by value; GetPosition auto-vivifies a
		// zero record, so pos is never nil.
		old = *pos
		curSize = pos.BaseSize
		delta := math.NewIntFromUint64(baseAmount).MulRaw(sign)

		fill := pos.ApplyFill(delta, price)
		pos.BaseSize = fill.Position.BaseSize
		pos.EntryQuote = fill.Position.EntryQuote
		newSize = pos.BaseSize
		realizedPnL = fill.RealizedPnL
		sideFlipped = fill.SideFlipped

		// Reject before persistence so we never store an out-of-bounds
		// position.
		if !isWithinPositionBounds(pos.BaseSize, pos.EntryQuote) {
			return errPositionOutOfBounds
		}
		return nil
	})
	if err != nil {
		return positionChangeResult{}, err
	}

	// OI contribution from this side: |new| - |old|.
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

// isWithinPositionBounds enforces |position| < 2^POSITION_SIZE_BITS
// and |entry_quote| < 2^ENTRY_QUOTE_BITS.
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
