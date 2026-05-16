package keeper

import (
	"context"

	sdk "github.com/cosmos/cosmos-sdk/types"

	perptypes "github.com/perpdex/perpdex-l1/types"
	accounttypes "github.com/perpdex/perpdex-l1/x/account/types"
	"github.com/perpdex/perpdex-l1/x/liquidation/types"
)

// EndBlocker walks every account and runs the auto-deleverage
// waterfall on every (account, market) tuple currently in
// FULL_LIQUIDATION or BANKRUPTCY:
//
//  1. Try to hand the position to the LLP / IF in ascending uPnL
//     order, gated by "post-takeover IF risk does not breach IF
//     IMR".
//  2. Positions the LLP cannot absorb (IMR breach) fall through to
//     ADL, where the deleverager-side pre-trade collateral assert
//     (`preCheckCollateral`) skips under-capitalised candidates and
//     advances to the next counterparty.
//
// Total work is bounded by `Params.MaxAdlAttemptsPerBlock`.
// PARTIAL_LIQUIDATION is serviced exclusively by MsgLiquidate;
// PRE_LIQUIDATION and HEALTHY are no-ops.
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
		// processAccount fans out per (account, market) using
		// the account's per-position margin mode; cross and
		// isolated health envelopes are read independently from
		// the same ranked list.
		if err := k.processAccount(ctx, a, &attemptsLeft, candCap); err != nil {
			sdkCtx.Logger().Error("liquidation: process account failed",
				"account", a.AccountIndex, "err", err)
		}
		return false
	})
}

// processAccount runs the per-account auto-deleverage logic. The
// account's positions are pre-sorted by ascending unrealized PnL
// (worst first) so the LLP is always offered the most negative
// position before any other; this is required by the spec — LLP
// takeover is meant to absorb the worst positions and let the rest
// fall through to ADL.
//
// For each ranked (account, market) tuple we re-read the relevant
// health envelope (cross for cross-margined positions, per-market
// isolated otherwise) right before invoking the LLP/ADL waterfall.
// Re-reading is necessary because a fill on an earlier (worse)
// market in the same iteration may have shifted the cross
// aggregate; the previous status snapshot is therefore not safe to
// reuse.
func (k Keeper) processAccount(
	ctx context.Context, a accounttypes.Account,
	attemptsLeft *uint32, candCap uint32,
) error {
	sdkCtx := sdk.UnwrapSDKContext(ctx)

	// Rank the victim's positions by ascending uPnL (worst first).
	// `rankVictimPositionsByUPnL` skips zero-size rows internally and
	// surfaces a non-nil error if any market's mark price cannot be
	// fetched (e.g. stalled oracle); see its docstring for why we
	// refuse to silently drop a row from the waterfall iterator.
	ranked, err := k.rankVictimPositionsByUPnL(ctx, a.AccountIndex)
	if err != nil {
		return err
	}

	for _, rp := range ranked {
		if attemptsLeft == nil || *attemptsLeft == 0 {
			return nil
		}
		marketIdx := rp.MarketIndex

		// Fresh health read: a sibling fill (LLP or ADL on a
		// worse market that we just processed) can have mutated
		// the cross aggregate, so cached statuses from earlier
		// in this account's iteration MUST NOT drive the next
		// market's decision. `healthEnvelopeFor` is the same
		// helper Liquidate/Deleverage use — kept in liquidate.go
		// so the cross-vs-isolated routing rule lives in one
		// place.
		status, err := k.healthEnvelopeFor(ctx, a.AccountIndex, marketIdx, rp.MarginMode)
		if err != nil {
			return err
		}
		if status != perptypes.HealthFullLiquidation && status != perptypes.HealthBankruptcy {
			continue
		}

		// Hand the position to the LLP first, gated by
		// SimulateRiskAfterTakeover so the LLP never breaches
		// its IMR. Anything refused falls through to ADL. Each
		// callee re-snapshots the victim internally so the
		// post-mutation state drives the next market's decision.
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
		// If both LLP and ADL refuse, the position remains and is
		// re-evaluated next block; there is no IF top-up sweep.
	}
	return nil
}
