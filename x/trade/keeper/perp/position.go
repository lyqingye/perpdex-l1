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
//
// Closed (issue #91): when the fill fully closes a position, `New`
// carries the **pre-close** snapshot returned by
// `accountKeeper.ClosePosition` (BaseSize still reflects the closed
// size and AllocatedMargin still reflects the pre-close pool) so the
// isolated-margin reconciliation can drain residual fields back to
// cross collateral. The downstream apply pipeline keys on
// `New.BaseSize == 0` to detect this.
type positionChangeResult struct {
	AccountIdx  uint64
	MarketIdx   uint32
	Old         accounttypes.AccountPosition
	New         accounttypes.AccountPosition
	OIDelta     int64
	SideFlipped bool
	RealizedPnL math.Int
	// Closed is true when this fill fully closed the position
	// (post-trade BaseSize == 0). Set so the downstream isolated /
	// cross handlers can branch without re-deriving `New.BaseSize`.
	Closed bool
}

// errPositionOutOfBounds signals that |position| or |entry_quote|
// would overflow POSITION_SIZE_BITS / ENTRY_QUOTE_BITS. Apply
// re-wraps it into Maker/Taker InvalidPosition for the matching loop.
var errPositionOutOfBounds = errors.New("trade: post-trade position out of bounds")

// applyPositionChange computes the new size / entry_quote / realized
// PnL for one side and dispatches the persistence through the
// matching lifecycle method on x/account:
//
//   - curSize == 0           → OpenPosition  (Opened event)
//   - newSize == 0           → ClosePosition (Closed event)
//   - side flipped           → ClosePosition + OpenPosition
//                              (Closed + Opened events; carries the
//                              old AllocatedMargin onto the new
//                              position so the isolated rebalance
//                              still re-margins from the residual
//                              pool, matching the pre-#91 semantics)
//   - same-side size change  → MutatePosition (Updated event)
//
// The quadrant math lives in `AccountPosition.ApplyFill` (shared with
// risk.SimulateRiskAfterTakeover); this wrapper drives the
// pre-classification, the bounds check, and the OI delta.
func (e Engine) applyPositionChange(ctx context.Context, accountIdx uint64, marketIdx uint32, price uint32, baseAmount uint64, sign int64) (positionChangeResult, error) {
	pre, err := e.accountKeeper.GetPosition(ctx, accountIdx, marketIdx)
	if err != nil {
		return positionChangeResult{}, err
	}
	delta := math.NewIntFromUint64(baseAmount).MulRaw(sign)
	fill := pre.ApplyFill(delta, price)

	if !isWithinPositionBounds(fill.Position.BaseSize, fill.Position.EntryQuote) {
		return positionChangeResult{}, errPositionOutOfBounds
	}

	res := positionChangeResult{
		AccountIdx:  accountIdx,
		MarketIdx:   marketIdx,
		Old:         pre,
		OIDelta:     fill.Position.BaseSize.Abs().Sub(pre.BaseSize.Abs()).Int64(),
		SideFlipped: fill.SideFlipped,
		RealizedPnL: fill.RealizedPnL,
	}

	switch {
	case pre.BaseSize.IsZero():
		// Pure open. OpenPosition pre-seeds leverage from any existing
		// leverage-only row; we only need to stamp BaseSize /
		// EntryQuote and the funding prefix-sum snapshot here.
		//
		// Funding prefix-sum: SettlePositionFunding short-circuits
		// on empty rows (issue #91), so the open path must seed
		// LastFundingRatePrefixSum from the market's current value;
		// otherwise the first post-open settlement would charge from
		// prefix=0 and over-bill the user.
		//
		// AllocatedMargin defaults to zero on the freshly opened row;
		// the downstream isolated rebalance will fund it from cross
		// collateral when applicable.
		md, err := e.marketKeeper.GetMarketDetails(ctx, marketIdx)
		if err != nil {
			return positionChangeResult{}, err
		}
		opened, err := e.accountKeeper.OpenPosition(ctx, accountIdx, marketIdx, func(p *accounttypes.AccountPosition) error {
			p.BaseSize = fill.Position.BaseSize
			p.EntryQuote = fill.Position.EntryQuote
			p.LastFundingRatePrefixSum = md.FundingRatePrefixSum
			return nil
		})
		if err != nil {
			return positionChangeResult{}, err
		}
		res.New = opened
		return res, nil

	case fill.Position.BaseSize.IsZero():
		// Pure close. ClosePosition returns the pre-close snapshot so
		// the isolated reconciliation downstream can drain residual
		// AllocatedMargin / EntryQuote / etc. back to cross.
		closed, err := e.accountKeeper.ClosePosition(ctx, accountIdx, marketIdx)
		if err != nil {
			return positionChangeResult{}, err
		}
		res.New = closed
		res.Closed = true
		return res, nil

	case fill.SideFlipped:
		// Flip = close old (with realized PnL on the closing leg) then
		// open new (residual leg). Carry the old position's leverage
		// + allocated_margin into the new open so the isolated
		// re-margin formula
		//   delta = posReq - (allocated + uPnL_new)
		// has the same starting allocated as the pre-#91
		// `UpdatePosition` codepath. The closing event fires with the
		// OLD position_id and the new event fires with the freshly
		// allocated id.
		closed, err := e.accountKeeper.ClosePosition(ctx, accountIdx, marketIdx)
		if err != nil {
			return positionChangeResult{}, err
		}
		opened, err := e.accountKeeper.OpenPosition(ctx, accountIdx, marketIdx, func(p *accounttypes.AccountPosition) error {
			p.BaseSize = fill.Position.BaseSize
			p.EntryQuote = fill.Position.EntryQuote
			// Carry over the user's allocated_margin so the isolated
			// rebalance can re-margin from the residual instead of
			// drain-then-fund (which would double-touch cross
			// collateral).
			p.AllocatedMargin = closed.AllocatedMargin
			// LastFundingRatePrefixSum is inherited so the next
			// settlement charges from the same prefix; the closing
			// leg already saw funding settled by the engine's
			// pre-fill SettlePositionFunding call.
			p.LastFundingRatePrefixSum = closed.LastFundingRatePrefixSum
			return nil
		})
		if err != nil {
			return positionChangeResult{}, err
		}
		res.New = opened
		return res, nil

	default:
		// Same-side increase / decrease (BaseSize != 0 throughout,
		// sign unchanged). MutatePosition enforces these invariants.
		updated, err := e.accountKeeper.MutatePosition(ctx, accountIdx, marketIdx, func(p *accounttypes.AccountPosition) error {
			p.BaseSize = fill.Position.BaseSize
			p.EntryQuote = fill.Position.EntryQuote
			return nil
		})
		if err != nil {
			return positionChangeResult{}, err
		}
		res.New = updated
		return res, nil
	}
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
