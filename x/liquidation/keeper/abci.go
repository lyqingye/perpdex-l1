package keeper

import (
	"context"
	"strconv"

	"cosmossdk.io/collections"

	sdk "github.com/cosmos/cosmos-sdk/types"

	perptypes "github.com/perpdex/perpdex-l1/types"
	accounttypes "github.com/perpdex/perpdex-l1/x/account/types"
	"github.com/perpdex/perpdex-l1/x/liquidation/types"
)

// EndBlocker walks every account and processes liquidation in three
// stages per (account, market):
//
//  1. Flag bookkeeping. Off-chain keeper bots use these flags to
//     decide which (account, market) tuples to target with
//     MsgLiquidate. PRE / HEALTHY accounts have their flags removed.
//  2. FULL_LIQUIDATION + BANKRUPTCY: same waterfall — try to hand the
//     position to the LLP / IF in ascending uPnL order, gated by
//     "post-takeover IF risk does not breach IF IMR". Positions the
//     LLP cannot absorb (IMR breach) fall through to ADL, where the
//     deleverager-side pre-trade collateral assert
//     (`preCheckCollateral`) skips under-capitalised candidates and
//     advances to the next counterparty. The Deleverage path
//     accepts FULL_LIQUIDATION and BANKRUPTCY indistinctly. The
//     chain caller bounds total work by
//     Params.MaxAdlAttemptsPerBlock.
func (k Keeper) EndBlocker(ctx context.Context) error {
	sdkCtx := sdk.UnwrapSDKContext(ctx)
	now := sdkCtx.BlockTime().UnixMilli()
	height := sdkCtx.BlockHeight()

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
		// status drives the per-account flag housekeeping; per
		// isolated position is then handled via its own per-market
		// isolated health envelope inside processAccount.
		if err := k.processAccount(ctx, a, height, now, &attemptsLeft, candCap); err != nil {
			sdkCtx.Logger().Error("liquidation: process account failed",
				"account", a.AccountIndex, "err", err)
		}
		return false
	})
}

// processAccount runs the per-account liquidation EndBlocker logic.
// Cross positions are flagged / liquidated against the cross health;
// each isolated position is flagged / liquidated against its own
// per-market isolated health.
//
// Health is re-read inside the FULL/BANKRUPTCY branch right before
// the LLP/ADL waterfall fires so a sibling-market fill that already
// healed the account in this iteration short-circuits before any
// (now-bogus) liquidation fill is quoted. autoADL also self-asserts
// FULL/BANKRUPTCY against its own fresh snapshot.
func (k Keeper) processAccount(
	ctx context.Context, a accounttypes.Account, height int64, now int64,
	attemptsLeft *uint32, candCap uint32,
) error {
	sdkCtx := sdk.UnwrapSDKContext(ctx)
	crossStatus, err := k.riskKeeper.GetHealthStatus(ctx, a.AccountIndex)
	if err != nil {
		return err
	}

	// PARTIAL+: write a flag for every CROSS market this account holds
	// a position in, so off-chain keeper bots can target MsgLiquidate.
	// PRE / HEALTHY clears stale cross flags for this account's cross
	// positions.
	healthyCross := crossStatus == perptypes.HealthHealthy || crossStatus == perptypes.HealthPreLiquidation

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

		if posStatus == perptypes.HealthHealthy || posStatus == perptypes.HealthPreLiquidation {
			_ = k.removeFlag(ctx, a.AccountIndex, marketIdx)
			return false
		}

		flag := types.LiquidationFlag{
			AccountIndex:   a.AccountIndex,
			MarketIndex:    marketIdx,
			FlaggedAtBlock: height,
			FlaggedAtTime:  now,
		}
		if err := k.setFlag(ctx, flag); err != nil {
			sdkCtx.Logger().Error("liquidation: set flag failed",
				"account", a.AccountIndex, "market", marketIdx, "err", err)
		}

		// FULL_LIQUIDATION + BANKRUPTCY: try the LLP first per the
		// spec ("LLP closes all of the user's positions by taking
		// them over"), gated by SimulateRiskAfterTakeover so the
		// LLP never breaches its IMR. Anything the LLP refuses
		// falls through to ADL. Each callee re-snapshots the victim
		// internally so the post-mutation state is what drives the
		// next market's decision.
		if attemptsLeft == nil || *attemptsLeft == 0 {
			return false
		}
		if posStatus == perptypes.HealthFullLiquidation || posStatus == perptypes.HealthBankruptcy {
			fresh, err := k.refreshHealth(ctx, a.AccountIndex, marketIdx, pos.MarginMode)
			if err != nil {
				iterErr = err
				return true
			}
			if fresh != perptypes.HealthFullLiquidation && fresh != perptypes.HealthBankruptcy {
				// A sibling fill in this account already healed
				// the envelope; do not quote a fill, and drop
				// the flag we just wrote so keeper bots do not
				// chase a recovered account for one extra block.
				_ = k.removeFlag(ctx, a.AccountIndex, marketIdx)
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

	if healthyCross {
		// Defensive: prune any stray cross-mode flags whose position
		// has since been closed (the per-loop branch above only
		// removes entries we still iterate over).
		_ = k.clearCrossFlags(ctx, a.AccountIndex)
	}
	// Re-read the cross status before emitting the flag event: an
	// LLP / ADL fill earlier in this iteration may have already
	// lifted the account back to HEALTHY, in which case the event
	// would otherwise carry the stale pre-iteration status — and any
	// cross flag we already wrote for an EARLIER market in this same
	// iteration would otherwise linger one extra block (the per-loop
	// healed-mid-iter branch only removes the CURRENT market's flag,
	// not earlier ones).
	if crossStatus != perptypes.HealthHealthy {
		finalCross, err := k.riskKeeper.GetHealthStatus(ctx, a.AccountIndex)
		if err != nil {
			return err
		}
		if finalCross == perptypes.HealthHealthy {
			_ = k.clearCrossFlags(ctx, a.AccountIndex)
		} else {
			sdkCtx.EventManager().EmitEvent(sdk.NewEvent(
				types.EventTypeLiquidationFlagged,
				sdk.NewAttribute(types.AttributeKeyAccountIndex, strconv.FormatUint(a.AccountIndex, 10)),
				sdk.NewAttribute(types.AttributeKeyStatus, strconv.FormatUint(uint64(finalCross), 10)),
			))
		}
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

// clearCrossFlags removes every (account, market) flag whose first key
// component matches `accIdx` and whose stored position is cross-mode.
// Called when the cross health is HEALTHY/PRE so stale cross flags
// from previous blocks are dropped, while leaving any isolated-mode
// flags intact (they are handled by the per-position branch above).
func (k Keeper) clearCrossFlags(ctx context.Context, accIdx uint64) error {
	rng := collections.NewPrefixedPairRange[uint64, uint32](accIdx)
	iter, err := k.Flags.Iterate(ctx, rng)
	if err != nil {
		return err
	}
	keys := []collections.Pair[uint64, uint32]{}
	for ; iter.Valid(); iter.Next() {
		k2, err := iter.Key()
		if err != nil {
			iter.Close()
			return err
		}
		keys = append(keys, k2)
	}
	iter.Close()
	for _, key := range keys {
		_, marketIdx := key.K1(), key.K2()
		pos, err := k.accountKeeper.GetPosition(ctx, accIdx, marketIdx)
		if err != nil {
			continue
		}
		if pos.MarginMode == perptypes.IsolatedMargin {
			continue
		}
		if err := k.removeFlag(ctx, accIdx, marketIdx); err != nil {
			return err
		}
	}
	return nil
}
