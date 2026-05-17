package keeper

import (
	"context"
	"errors"
	"fmt"
	"sort"

	"cosmossdk.io/math"

	sdk "github.com/cosmos/cosmos-sdk/types"

	perptypes "github.com/perpdex/perpdex-l1/types"
	accounttypes "github.com/perpdex/perpdex-l1/x/account/types"
	"github.com/perpdex/perpdex-l1/x/liquidation/types"
)

// tryLLPAbsorb implements the "LLP picks up positions in ascending
// order of unrealized PnL, only when doing so keeps the LLP TAV >= IMR"
// rule for ONE (victim, market) tuple. The caller (processAccount) is
// responsible for invoking this function in worst-uPnL-first order:
// see `rankVictimPositionsByUPnL`. This keeper-level helper performs
// NO ranking itself, so it cannot reject a position just because a
// worse one exists in another market — that responsibility is owned
// by the outer loop.
//
// Returns true iff the targeted position was fully absorbed; the
// caller skips ADL on a true return. False return means one of:
//   - LLP would have breached IMR on takeover (simulated post-state),
//   - LLP / IF is frozen, in wind-down, or not provisioned,
//   - LLP attempts are exhausted for this block, or
//   - the snapshot reports a zero-size position (defensive guard;
//     see the BaseSize.IsZero() short-circuit below).
//
// In all of these false-return cases the caller falls back to ADL
// for the residual size. A non-nil error indicates an upstream
// invariant violation (e.g., LLP / IF position misconfigured as
// isolated); the EndBlocker logs and still falls through to autoADL.
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

	// Snapshot the victim's targeted position fresh: any earlier
	// LLP/ADL fill in this account's iteration may have shifted the
	// cross aggregate, and the snapshot's ZeroPrice MUST come from
	// the post-mutation TAV/MMR.
	snap, err := k.riskKeeper.GetLiquidationRiskSnapshot(ctx, victim, marketIdx)
	if err != nil {
		return false, err
	}
	// Defensive zero-size guard. rankVictimPositionsByUPnL only
	// emits non-zero rows, and a sibling-market fill within the
	// same processAccount iteration cannot zero THIS market's base
	// size (perp trades only mutate their own market). The guard
	// remains as protection against future call sites that might
	// invoke tryLLPAbsorb outside the ranked-iteration contract,
	// and against any pre-snapshot side effect that closes the
	// position before the snapshot is taken.
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
			// INVARIANT VIOLATION. The current `Deleverage`
			// implementation does NOT raise
			// ErrInsufficientCollateral on the IF / Pool
			// deleverager path (the user-deleverager
			// collateral guard was removed by F6), so this
			// branch is unreachable. If a future change
			// re-introduces a collateral assert against the
			// IF and silently flips it into this branch, the
			// LLP path would degrade to "absorbing nothing"
			// without surfacing the regression. Surface the
			// violation as a hard error so processAccount
			// logs it loudly — processAccount already
			// fall-throughs to autoADL on a non-nil error
			// from tryLLPAbsorb (see x/liquidation/keeper/abci.go
			// processAccount), so the block still advances.
			return false, fmt.Errorf(
				"INVARIANT VIOLATION: LLP %d hit ErrInsufficientCollateral on market %d "+
					"(Deleverage is expected to skip the collateral guard for IF/Pool "+
					"deleveragers; see x/liquidation/keeper/liquidate.go Deleverage): %w",
				perptypes.InsuranceFundOperatorAccountIdx, marketIdx, err,
			)
		}
		sdk.UnwrapSDKContext(ctx).Logger().Error("liquidation: LLP absorb failed",
			"victim", victim, "market", marketIdx, "err", err)
		return false, err
	}
	*attemptsLeft--
	return true, nil
}

// rankedPosition is the per-position summary `rankVictimPositionsByUPnL`
// emits for the EndBlocker. It carries everything `processAccount`
// needs to drive the LLP/ADL waterfall in worst-uPnL-first order
// without re-reading the victim's position table per market:
//   - MarketIndex: which market the row refers to (waterfall target).
//   - MarginMode:  picks the right health envelope (cross vs isolated)
//     in `healthEnvelopeFor`. Cross and isolated rows are co-ranked
//     in a single uPnL-ascending list (see rankVictimPositionsByUPnL
//     docstring for the rationale and limitations).
//   - UnrealizedPnL: sort key (ascending).
type rankedPosition struct {
	MarketIndex   uint32
	MarginMode    uint32
	UnrealizedPnL math.Int
}

// rankVictimPositionsByUPnL returns the victim's non-zero positions
// sorted by ascending unrealized PnL (worst first), as the spec
// requires for LLP takeover. uPnL is derived from `pos.UnrealizedPnL`
// against the per-market mark price; ranking does NOT materialise a
// risk snapshot, only the mark price is needed.
//
// # Cross + isolated mixed ordering
//
// Both cross and isolated positions are surfaced in the same list.
// Per-position health is still read via the correct envelope through
// `healthEnvelopeFor(..., MarginMode)` before any waterfall action,
// so an isolated position cannot accidentally drive cross-bucket
// decisions. Mixing the two in one uPnL sort is acceptable because
// each ranked entry is handled in its own (account, market) waterfall
// step that has no side effects on the OTHER bucket: a fill on a
// cross position settles realised PnL into the cross collateral
// only, and a fill on an isolated position settles realised PnL into
// that isolated position's AllocatedMargin only. Spec language
// ("LLP closes all of the user's positions by taking them over")
// does NOT prescribe a cross-first / isolated-second order, so the
// uPnL sort is sufficient.
//
// # Source of truth for EndBlocker iteration
//
// The returned list is the SINGLE iteration source `processAccount`
// uses; any market dropped here is skipped by the waterfall for this
// block. Mark price lookup failures therefore must NOT silently
// drop the row — instead they are surfaced as errors so the outer
// EndBlocker logs them and a follow-up block can retry.
func (k Keeper) rankVictimPositionsByUPnL(ctx context.Context, victim uint64) ([]rankedPosition, error) {
	out := []rankedPosition{}
	var iterErr error
	if err := k.accountKeeper.IterateAccountPositions(ctx, victim, func(pos accounttypes.AccountPosition) bool {
		if pos.BaseSize.IsZero() {
			return false
		}
		// `IterateAccountPositions` yields each (victim, market)
		// tuple exactly once, so a per-call markPrice cache would
		// always miss — direct lookup is correct AND simpler.
		markPrice, _, err := k.marketKeeper.GetMarkPriceAndDetails(ctx, pos.MarketIndex)
		if err != nil {
			// Surface the error: this list is the EndBlocker's
			// sole iteration source, silently dropping a market
			// would hide a stalled oracle until residual debt
			// alerts fire. Stop the walk and let the caller log.
			iterErr = err
			return true
		}
		out = append(out, rankedPosition{
			MarketIndex:   pos.MarketIndex,
			MarginMode:    pos.MarginMode,
			UnrealizedPnL: pos.UnrealizedPnL(markPrice),
		})
		return false
	}); err != nil {
		return nil, err
	}
	if iterErr != nil {
		return nil, iterErr
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
