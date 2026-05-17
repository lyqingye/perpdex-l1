package keeper

import (
	"context"

	sdk "github.com/cosmos/cosmos-sdk/types"

	perptypes "github.com/perpdex/perpdex-l1/types"
	accounttypes "github.com/perpdex/perpdex-l1/x/account/types"
	"github.com/perpdex/perpdex-l1/x/liquidation/types"
)

// EndBlocker runs the auto-deleverage waterfall on every
// FULL_LIQUIDATION / BANKRUPTCY (account, market):
//
//  1. tryLLPAbsorb in ascending-uPnL order, gated by post-takeover
//     LLP TAV >= IMR.
//  2. Positions the LLP cannot absorb fall through to autoADL. The
//     trade engine's IsValidRiskChangeFrom (on both maker and taker)
//     is the sole counterparty health gate; an under-capitalised
//     candidate surfaces as ErrTakerRiskRegression and autoADL
//     advances to the next.
//
// Bounded by Params.MaxAdlAttemptsPerBlock. PARTIAL is serviced by
// MsgLiquidate; PRE/HEALTHY are no-ops.
func (k Keeper) EndBlocker(ctx context.Context) error {
	sdkCtx := sdk.UnwrapSDKContext(ctx)

	params, err := k.Params.Get(ctx)
	if err != nil {
		return err
	}
	attemptsLeft := params.MaxAdlAttemptsPerBlock
	candCap := params.MaxAdlCandidatesPerVictim
	if candCap == 0 {
		candCap = types.DefaultMaxADLCandidatesPerVictim
	}

	return k.accountKeeper.IterateAccounts(ctx, func(a accounttypes.Account) bool {
		if a.AccountIndex == perptypes.InsuranceFundOperatorAccountIdx {
			return false
		}
		// Cross/isolated envelopes are read independently per market
		// off the same ranked list.
		if err := k.processAccount(ctx, a, &attemptsLeft, candCap); err != nil {
			sdkCtx.Logger().Error("liquidation: process account failed",
				"account", a.AccountIndex, "err", err)
		}
		return false
	})
}

// processAccount runs the per-account waterfall. Positions are
// pre-sorted ascending uPnL (worst first) so the LLP is always
// offered the most negative position; health is re-read per market
// because an earlier fill in the same iteration can shift the cross
// aggregate.
func (k Keeper) processAccount(
	ctx context.Context, a accounttypes.Account,
	attemptsLeft *uint32, candCap uint32,
) error {
	sdkCtx := sdk.UnwrapSDKContext(ctx)

	// Rank ascending uPnL (worst first). Skips zero-size rows; a
	// mark-price failure surfaces as an error (silently dropping a
	// row would hide a stalled oracle).
	ranked, err := k.rankVictimPositionsByUPnL(ctx, a.AccountIndex)
	if err != nil {
		return err
	}

	for _, rp := range ranked {
		if attemptsLeft == nil || *attemptsLeft == 0 {
			return nil
		}
		marketIdx := rp.MarketIndex

		// Fresh health read: a sibling fill earlier in this
		// account's iteration may have shifted the cross aggregate,
		// so cached statuses are unsafe.
		status, err := k.healthEnvelopeFor(ctx, a.AccountIndex, marketIdx, rp.MarginMode)
		if err != nil {
			return err
		}
		if status != perptypes.HealthFullLiquidation && status != perptypes.HealthBankruptcy {
			continue
		}

		// LLP first, gated by SimulateRiskAfterTakeover; refusals
		// fall through to ADL. Each callee re-snapshots the victim
		// so post-mutation state drives the next market's decision.
		absorbed, err := k.tryLLPAbsorb(ctx, a.AccountIndex, marketIdx, attemptsLeft)
		if err != nil {
			sdkCtx.Logger().Error("liquidation: LLP absorb failed",
				"victim", a.AccountIndex, "market", marketIdx, "err", err)
		}
		if !absorbed {
			if err := k.autoADL(ctx, a.AccountIndex, marketIdx, candCap, attemptsLeft); err != nil {
				sdkCtx.Logger().Error("liquidation: auto-adl failed",
					"victim", a.AccountIndex, "market", marketIdx, "err", err)
			}
		}
		// If both refuse, the position is re-evaluated next block;
		// there is no IF top-up sweep.
	}
	return nil
}
