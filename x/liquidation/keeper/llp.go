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
	risktypes "github.com/perpdex/perpdex-l1/x/risk/types"
)

// tryLLPAbsorb implements the Lighter "LLP picks up positions in
// ascending order of unrealized PnL, only when doing so keeps the LLP
// TAV >= IMR" rule. Called once per victim per FULL_LIQUIDATION cycle
// — it ranks the victim's OWN positions by uPnL and offers the worst
// (most negative) one to the LLP first.
//
// Returns true iff the targeted position was fully absorbed; the
// caller skips ADL on a true return. False return means the LLP would
// have breached IMR or is frozen / nonexistent — caller falls back to
// ADL for the residual size.
//
// `victimRP` lets the EndBlocker hand in the cross / isolated risk
// parameters it already fetched for `victim`, sparing one
// ComputeRiskInfo / ComputeIsolatedRisk inside the ZP computation.
// Pass nil when the caller has no cached state.
func (k Keeper) tryLLPAbsorb(
	ctx context.Context,
	victim uint64,
	marketIdx uint32,
	attemptsLeft *uint32,
	victimRP *risktypes.RiskParameters,
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

	pos, err := k.accountKeeper.GetPosition(ctx, victim, marketIdx)
	if err != nil || pos.Position.IsZero() {
		return false, err
	}
	size := pos.Position.Abs()
	if !size.IsPositive() {
		return false, nil
	}

	// Prefetch market state so both ZP and the takeover simulation
	// can reuse it. Each helper call below is pure math.
	mark, md, err := k.riskKeeper.GetMarkAndMarketDetails(ctx, marketIdx)
	if err != nil {
		return false, err
	}
	victimParams, err := k.resolveVictimRiskParams(ctx, victim, marketIdx, pos, victimRP)
	if err != nil {
		return false, err
	}
	zeroPrice := k.riskKeeper.ComputeZeroPrice(pos, mark, md, victimParams.TotalAccountValue, victimParams.MaintenanceMarginRequirement)

	// LLP IMR check: simulate the takeover and require the LLP's
	// post-state TAV >= IMR. The takeover delta is the position the
	// LLP will inherit (opposite sign of victim, since LLP is the
	// taker that offsets the victim's exposure).
	llpDelta := pos.Position.Neg()
	llpPos, err := k.accountKeeper.GetPosition(ctx, perptypes.InsuranceFundOperatorAccountIdx, marketIdx)
	if err != nil {
		return false, err
	}
	if llpPos.MarginMode == perptypes.IsolatedMargin {
		// LLP positions are always cross; refuse rather than silently
		// mis-simulate. The fallback is ADL.
		return false, nil
	}
	llpRi, err := k.riskKeeper.ComputeRiskInfo(ctx, perptypes.InsuranceFundOperatorAccountIdx)
	if err != nil {
		return false, err
	}
	llpCurrent := risktypes.RiskParameters{}
	if llpRi.CurrentRiskParameters != nil {
		llpCurrent = *llpRi.CurrentRiskParameters
	}
	postRP := k.riskKeeper.ApplySimulatedTakeover(llpPos, llpCurrent, mark, md, llpDelta, zeroPrice)
	if postRP.TotalAccountValue.LT(postRP.InitialMarginRequirement) {
		// LLP would breach its initial margin; reject and let ADL
		// handle the position.
		return false, nil
	}

	if err := k.Deleverage(ctx, victim, marketIdx, perptypes.InsuranceFundOperatorAccountIdx, size.Uint64()); err != nil {
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

// rankedPosition is one row in the ranked-victim-positions list used
// by tryLLPAbsorb to enforce ascending-uPnL ordering.
type rankedPosition struct {
	MarketIndex   uint32
	UnrealizedPnL math.Int
}

// rankVictimPositionsByUPnL returns the victim's non-zero positions
// sorted by ascending unrealized PnL (worst first), as the Lighter
// spec requires for LLP takeover. Mark prices are fetched once per
// distinct MarketIndex encountered and reused; uPnL is derived
// directly from the iterated position via `pos.UnrealizedPnL(mark)`,
// avoiding a second GetPosition round-trip.
func (k Keeper) rankVictimPositionsByUPnL(ctx context.Context, victim uint64) ([]rankedPosition, error) {
	out := []rankedPosition{}
	marks := map[uint32]uint32{}
	if err := k.accountKeeper.IterateAccountPositions(ctx, victim, func(pos accounttypes.AccountPosition) bool {
		if pos.Position.IsZero() {
			return false
		}
		mark, ok := marks[pos.MarketIndex]
		if !ok {
			m, _, err := k.riskKeeper.GetMarkAndMarketDetails(ctx, pos.MarketIndex)
			if err != nil {
				// Stale oracle: skip this market in the ranking,
				// the outer EndBlocker will surface the error
				// separately.
				return false
			}
			mark = m
			marks[pos.MarketIndex] = mark
		}
		out = append(out, rankedPosition{
			MarketIndex:   pos.MarketIndex,
			UnrealizedPnL: pos.UnrealizedPnL(mark),
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
