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

// tryLLPAbsorb handles ONE (victim, market) takeover: the LLP picks
// up the position iff its post-takeover TAV >= IMR. Worst-uPnL-first
// ranking is owned by the caller (processAccount).
//
// Returns true on full absorption (caller skips ADL). False on:
// LLP IMR breach, LLP/IF frozen/missing, attempts exhausted, or a
// defensive zero-size guard. Non-nil error signals an upstream
// invariant violation (e.g. LLP misconfigured as isolated); the
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
		return false, nil // IF not provisioned.
	}
	if err := accounttypes.EnsureActive(llp.PublicPoolInfo); err != nil {
		// LLP frozen / wind-down / missing info → waterfall continues.
		return false, nil
	}

	// Fresh snapshot: any earlier fill in this iteration may have
	// shifted the cross aggregate, and ZeroPrice MUST reflect the
	// post-mutation TAV/MMR.
	snap, err := k.riskKeeper.GetLiquidationRiskSnapshot(ctx, victim, marketIdx)
	if err != nil {
		return false, err
	}
	// Defensive: ranked iteration only emits non-zero rows and
	// sibling-market fills cannot zero this market, but a future
	// caller may invoke us outside that contract.
	if snap.Position.BaseSize.IsZero() {
		return false, nil
	}
	size := snap.Position.BaseSize.Abs()
	if !size.IsPositive() {
		return false, nil
	}

	// IMR gate: simulate the takeover and require post.TAV >= post.IMR.
	// Delta is opposite-sign of victim (LLP offsets the exposure).
	// SimulateRiskAfterTakeover errs on isolated targets (LLP/IF MUST
	// be cross); we surface the error rather than silently swallowing.
	llpDelta := snap.Position.BaseSize.Neg()
	postRP, err := k.riskKeeper.SimulateRiskAfterTakeover(
		ctx, perptypes.InsuranceFundOperatorAccountIdx, marketIdx, llpDelta, snap.ZeroPrice,
	)
	if err != nil {
		return false, err
	}
	if postRP.TotalAccountValue.LT(postRP.InitialMarginRequirement) {
		// LLP would breach IMR; let ADL handle the position.
		return false, nil
	}

	if err := k.Deleverage(
		ctx, victim, marketIdx, perptypes.InsuranceFundOperatorAccountIdx, size.Uint64(),
		WithDeleverageSource(DeleverageSourceLLP),
	); err != nil {
		if errors.Is(err, types.ErrInsufficientCollateral) {
			// INVARIANT VIOLATION: Deleverage skips the collateral
			// guard for IF/Pool deleveragers, so this branch is
			// unreachable. A future regression that re-introduces
			// the assert would degrade LLP to "absorb nothing" —
			// surface as a hard error. processAccount logs and
			// still falls through to autoADL.
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

// rankedPosition is one entry of the worst-uPnL-first list
// processAccount drives. MarginMode picks the cross/isolated health
// envelope in healthEnvelopeFor; cross and isolated rows are
// co-ranked (see rankVictimPositionsByUPnL).
type rankedPosition struct {
	MarketIndex   uint32
	MarginMode    uint32
	UnrealizedPnL math.Int
}

// rankVictimPositionsByUPnL returns non-zero positions sorted by
// ascending uPnL (worst first). uPnL is derived directly from
// pos.UnrealizedPnL — no risk snapshot needed, only the mark.
//
// Cross and isolated positions are co-ranked: each ranked entry is
// handled in its own (account, market) step, fills only touch their
// own bucket (cross collateral vs isolated AllocatedMargin), and
// healthEnvelopeFor still routes per MarginMode. The spec does not
// prescribe a cross-first ordering, so uPnL sort alone is sufficient.
//
// This is the SINGLE iteration source for processAccount; mark-price
// failures MUST surface as errors (silently dropping a row would hide
// a stalled oracle until residual-debt alerts fire).
func (k Keeper) rankVictimPositionsByUPnL(ctx context.Context, victim uint64) ([]rankedPosition, error) {
	out := []rankedPosition{}
	var iterErr error
	if err := k.accountKeeper.IterateAccountPositions(ctx, victim, func(pos accounttypes.AccountPosition) bool {
		if pos.BaseSize.IsZero() {
			return false
		}
		// IterateAccountPositions yields each market once, so a
		// per-call markPrice cache would always miss.
		markPrice, _, err := k.marketKeeper.GetMarkPriceAndDetails(ctx, pos.MarketIndex)
		if err != nil {
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
		// Ascending uPnL; market_index tiebreak for determinism.
		if !out[i].UnrealizedPnL.Equal(out[j].UnrealizedPnL) {
			return out[i].UnrealizedPnL.LT(out[j].UnrealizedPnL)
		}
		return out[i].MarketIndex < out[j].MarketIndex
	})
	return out, nil
}
