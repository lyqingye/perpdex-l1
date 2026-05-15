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
// The chain caller bounds total work by
// `Params.MaxAdlAttemptsPerBlock`. PARTIAL_LIQUIDATION accounts are
// intentionally NOT processed here: that tier is keeper-bot
// territory via MsgLiquidate. PRE_LIQUIDATION and HEALTHY do
// nothing.
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
		// We process cross and isolated health independently. Cross
		// status drives any per-account decision; each isolated
		// position is handled via its own per-market isolated
		// health envelope inside processAccount.
		if err := k.processAccount(ctx, a, &attemptsLeft, candCap); err != nil {
			sdkCtx.Logger().Error("liquidation: process account failed",
				"account", a.AccountIndex, "err", err)
		}
		return false
	})
}

// processAccount runs the per-account auto-deleverage logic. For
// each (account, market) with a non-zero position, the relevant
// health envelope (cross for cross-margined positions, per-market
// isolated otherwise) is consulted; FULL_LIQUIDATION and BANKRUPTCY
// positions enter the LLP/ADL waterfall. PARTIAL_LIQUIDATION is
// skipped — those are MsgLiquidate's responsibility.
//
// Health is re-read inside the FULL/BANKRUPTCY branch right before
// the LLP/ADL waterfall fires so a sibling-market fill that already
// healed the account in this iteration short-circuits before any
// (now-bogus) liquidation fill is quoted. autoADL also self-asserts
// FULL/BANKRUPTCY against its own fresh snapshot.
func (k Keeper) processAccount(
	ctx context.Context, a accounttypes.Account,
	attemptsLeft *uint32, candCap uint32,
) error {
	sdkCtx := sdk.UnwrapSDKContext(ctx)
	crossStatus, err := k.riskKeeper.GetHealthStatus(ctx, a.AccountIndex)
	if err != nil {
		return err
	}

	var iterErr error
	if err := k.accountKeeper.IterateAccountPositions(ctx, a.AccountIndex, func(pos accounttypes.AccountPosition) bool {
		if pos.BaseSize.IsZero() {
			return false
		}
		marketIdx := pos.MarketIndex
		var posStatus uint32
		if pos.MarginMode == perptypes.IsolatedMargin {
			s, err := k.riskKeeper.GetIsolatedHealthStatus(ctx, a.AccountIndex, marketIdx)
			if err != nil {
				iterErr = err
				return true
			}
			posStatus = s
		} else {
			posStatus = crossStatus
		}

		if posStatus != perptypes.HealthFullLiquidation && posStatus != perptypes.HealthBankruptcy {
			return false
		}

		if attemptsLeft == nil || *attemptsLeft == 0 {
			return false
		}
		// FULL_LIQUIDATION + BANKRUPTCY: try the LLP first
		// ("LLP closes all of the user's positions by taking
		// them over"), gated by SimulateRiskAfterTakeover so the
		// LLP never breaches its IMR. Anything the LLP refuses
		// falls through to ADL. Each callee re-snapshots the
		// victim internally so the post-mutation state is what
		// drives the next market's decision.
		fresh, err := k.refreshHealth(ctx, a.AccountIndex, marketIdx, pos.MarginMode)
		if err != nil {
			iterErr = err
			return true
		}
		if fresh != perptypes.HealthFullLiquidation && fresh != perptypes.HealthBankruptcy {
			// A sibling fill in this account already healed
			// the envelope; nothing to do for this market.
			return false
		}
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
		// No silent IF top-up of residual negative collateral.
		// "Absorption" is the LLP/IF deleverage trade itself; if
		// `tryLLPAbsorb` rejected (IMR breach) and `autoADL` could
		// not find counterparties, the position simply remains and
		// is re-evaluated next block — there is no silent IF
		// top-up sweep.
		return false
	}); err != nil {
		return err
	}
	if iterErr != nil {
		return iterErr
	}
	return nil
}

// refreshHealth re-reads the account's relevant health envelope
// (cross or isolated) for `(accIdx, marketIdx)`. Used right before
// the LLP / ADL waterfall fires so prior fills in the same
// processAccount iteration cannot leave the trigger pointing at a
// stale FULL / BANKRUPTCY status.
func (k Keeper) refreshHealth(
	ctx context.Context, accIdx uint64, marketIdx uint32, marginMode uint32,
) (uint32, error) {
	if marginMode == perptypes.IsolatedMargin {
		return k.riskKeeper.GetIsolatedHealthStatus(ctx, accIdx, marketIdx)
	}
	return k.riskKeeper.GetHealthStatus(ctx, accIdx)
}
