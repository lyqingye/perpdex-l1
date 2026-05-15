package keeper

import (
	"context"
	"errors"
	"sort"

	"cosmossdk.io/math"

	sdk "github.com/cosmos/cosmos-sdk/types"

	perptypes "github.com/perpdex/perpdex-l1/types"
	accounttypes "github.com/perpdex/perpdex-l1/x/account/types"
	"github.com/perpdex/perpdex-l1/x/liquidation/types"
)

// tryLLPAbsorb implements the "LLP picks up positions in ascending
// order of unrealized PnL, only when doing so keeps the LLP TAV >= IMR"
// rule. Called once per victim per FULL_LIQUIDATION cycle — it ranks
// the victim's OWN positions by uPnL and offers the worst (most
// negative) one to the LLP first.
//
// Returns true iff the targeted position was fully absorbed; the
// caller skips ADL on a true return. False return means the LLP would
// have breached IMR, is frozen / nonexistent, or the targeted
// position is not the worst one yet — caller falls back to ADL for
// the residual size. A non-nil error indicates an upstream invariant
// violation (e.g., LLP / IF position misconfigured as isolated); the
// EndBlocker logs and still falls through to autoADL.
func (k Keeper) tryLLPAbsorb(
	ctx context.Context,
	victim uint64,
	marketIdx uint32,
	attemptsLeft *uint32,
) (bool, error) {
	if attemptsLeft == nil || *attemptsLeft == 0 {
		return false, nil
	}
	llp, err := k.accountKeeper.GetAccount(ctx, perptypes.InsuranceFundOperatorAccountIdx)
	if err != nil {
		return false, nil // IF not provisioned: silently skip.
	}
	if err := accounttypes.EnsureActive(llp.PublicPoolInfo); err != nil {
		// LLP not active (frozen / wind-down / missing info): silently
		// skip; absorbing falls back to the next stage in the waterfall.
		return false, nil
	}

	// Build the ranked queue of the VICTIM's positions (worst uPnL
	// first). We only attempt the targeted `marketIdx` here; the
	// outer loop walks every market in order and only invokes us
	// when this one is FULL_LIQUIDATION. We still consult the rank
	// to make sure we are not trying to absorb the BEST position
	// before the WORST has been offered — which would let the LLP
	// cherry-pick winners and leave bad positions for ADL.
	worstFirst, err := k.rankVictimPositionsByUPnL(ctx, victim)
	if err != nil {
		return false, err
	}
	if len(worstFirst) > 0 && worstFirst[0].MarketIndex != marketIdx {
		// A worse position exists in another market; defer this
		// market until that one is processed (next EndBlocker cycle
		// — accounts/markets are iterated deterministically).
		return false, nil
	}

	// Snapshot the victim's targeted position fresh: any earlier
	// LLP/ADL fill in this account's iteration may have shifted the
	// cross aggregate, and the snapshot's ZeroPrice MUST come from
	// the post-mutation TAV/MMR.
	snap, err := k.riskKeeper.GetLiquidationRiskSnapshot(ctx, victim, marketIdx)
	if err != nil {
		return false, err
	}
	if snap.Position.BaseSize.IsZero() {
		return false, nil
	}
	size := snap.Position.BaseSize.Abs()
	if !size.IsPositive() {
		return false, nil
	}

	// LLP IMR check: simulate the takeover and require the LLP's
	// post-state TAV >= IMR. The takeover delta is the position the
	// LLP will inherit (opposite sign of victim, since LLP is the
	// taker that offsets the victim's exposure). The wrapper
	// re-reads the LLP's cross state, so even if a sibling market
	// just absorbed against the LLP earlier in this account's
	// iteration the gate still uses fresh TAV / IMR.
	//
	// SimulateRiskAfterTakeover refuses isolated targets with an
	// error (LLP / IF positions MUST be cross). We surface that as a
	// non-nil error rather than swallowing it as a silent fallback —
	// processAccount logs the misconfiguration and still falls
	// through to autoADL.
	llpDelta := snap.Position.BaseSize.Neg()
	postRP, err := k.riskKeeper.SimulateRiskAfterTakeover(
		ctx, perptypes.InsuranceFundOperatorAccountIdx, marketIdx, llpDelta, snap.ZeroPrice,
	)
	if err != nil {
		return false, err
	}
	if postRP.TotalAccountValue.LT(postRP.InitialMarginRequirement) {
		// LLP would breach its initial margin; reject and let ADL
		// handle the position.
		return false, nil
	}

	if err := k.Deleverage(
		ctx, victim, marketIdx, perptypes.InsuranceFundOperatorAccountIdx, size.Uint64(),
		WithDeleverageSource(DeleverageSourceLLP),
	); err != nil {
		if errors.Is(err, types.ErrInsufficientCollateral) {
			// Defensive: `Deleverage` skips the deleverager
			// pre-trade collateral assert when the deleverager is
			// the IF (`isInsuranceFund`), so this branch is not
			// expected to fire on the LLP path. Keep the graceful
			// fallback in case a future change re-introduces a
			// collateral guard against the IF — we still want
			// EndBlocker to advance to autoADL rather than abort
			// the entire block.
			sdk.UnwrapSDKContext(ctx).Logger().Info("liquidation: LLP absorb skipped (insufficient collateral)",
				"victim", victim, "market", marketIdx, "err", err)
			return false, nil
		}
		sdk.UnwrapSDKContext(ctx).Logger().Error("liquidation: LLP absorb failed",
			"victim", victim, "market", marketIdx, "err", err)
		return false, err
	}
	*attemptsLeft--
	return true, nil
}

type rankedPosition struct {
	MarketIndex   uint32
	UnrealizedPnL math.Int
}

// rankVictimPositionsByUPnL returns the victim's non-zero positions
// sorted by ascending unrealized PnL (worst first), as the spec
// requires for LLP takeover. Mark prices are fetched once per distinct
// MarketIndex encountered and reused; uPnL is derived directly from
// the iterated position via `pos.UnrealizedPnL(markPrice)`.
//
// Ranking does NOT materialise a full risk snapshot per position —
// only the markPrice is needed to score uPnL, and a snapshot's extra cross
// aggregation would be O(positions^2) for the same victim.
func (k Keeper) rankVictimPositionsByUPnL(ctx context.Context, victim uint64) ([]rankedPosition, error) {
	out := []rankedPosition{}
	marks := map[uint32]uint32{}
	if err := k.accountKeeper.IterateAccountPositions(ctx, victim, func(pos accounttypes.AccountPosition) bool {
		if pos.BaseSize.IsZero() {
			return false
		}
		markPrice, ok := marks[pos.MarketIndex]
		if !ok {
			m, _, err := k.marketKeeper.GetMarkPriceAndDetails(ctx, pos.MarketIndex)
			if err != nil {
				// Stale markPrice: skip this market in the ranking,
				// the outer EndBlocker will surface the error
				// separately.
				return false
			}
			markPrice = m
			marks[pos.MarketIndex] = markPrice
		}
		out = append(out, rankedPosition{
			MarketIndex:   pos.MarketIndex,
			UnrealizedPnL: pos.UnrealizedPnL(markPrice),
		})
		return false
	}); err != nil {
		return nil, err
	}
	sort.Slice(out, func(i, j int) bool {
		// Ascending uPnL (most negative first); deterministic
		// market_index tiebreak.
		if !out[i].UnrealizedPnL.Equal(out[j].UnrealizedPnL) {
			return out[i].UnrealizedPnL.LT(out[j].UnrealizedPnL)
		}
		return out[i].MarketIndex < out[j].MarketIndex
	})
	return out, nil
}
