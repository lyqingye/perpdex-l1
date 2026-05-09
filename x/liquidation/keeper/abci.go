package keeper

import (
	"context"
	"strconv"

	"cosmossdk.io/collections"

	sdk "github.com/cosmos/cosmos-sdk/types"

	perptypes "github.com/perpdex/perpdex-l1/types"
	accounttypes "github.com/perpdex/perpdex-l1/x/account/types"
	"github.com/perpdex/perpdex-l1/x/liquidation/types"
	riskkeeper "github.com/perpdex/perpdex-l1/x/risk/keeper"
	risktypes "github.com/perpdex/perpdex-l1/x/risk/types"
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
//     (`is_deleverager_has_enough_cross_collateral`) skips
//     under-capitalised candidates and advances to the next
//     counterparty. Mirrors Lighter `internal_deleverage.rs`
//     accepting both health states indistinctly. The chain caller
//     bounds total work by Params.MaxAdlAttemptsPerBlock.
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
		// isolated position is then handled via the same routine
		// against ComputeIsolatedRisk.
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
// Risk-info dedup: this function fetches `ComputeRiskInfo(account)`
// (cross) and per-isolated-position `ComputeIsolatedRisk` directly
// instead of going through `GetHealthStatus` /
// `GetIsolatedHealthStatus` wrappers (which throw away the
// RiskParameters and force `tryLLPAbsorb` / `autoADL` to recompute
// them). The cached params are passed down to those callees so they
// don't re-aggregate the account's positions per (account, market)
// triggered.
func (k Keeper) processAccount(
	ctx context.Context, a accounttypes.Account, height int64, now int64,
	attemptsLeft *uint32, candCap uint32,
) error {
	sdkCtx := sdk.UnwrapSDKContext(ctx)
	crossRi, err := k.riskKeeper.ComputeRiskInfo(ctx, a.AccountIndex)
	if err != nil {
		return err
	}
	var crossRP *risktypes.RiskParameters
	crossStatus := perptypes.HealthHealthy
	if crossRi.CurrentRiskParameters != nil {
		rp := *crossRi.CurrentRiskParameters
		crossRP = &rp
		crossStatus = riskkeeper.ClassifyHealth(rp)
	}

	// PARTIAL+: write a flag for every CROSS market this account holds
	// a position in, so off-chain keeper bots can target MsgLiquidate.
	// PRE / HEALTHY clears stale cross flags for this account's cross
	// positions.
	healthyCross := crossStatus == perptypes.HealthHealthy || crossStatus == perptypes.HealthPreLiquidation

	// Walk only persisted position rows; the legacy 0..MaxPerpsMarketIndex
	// scan generated up to 256 GetPosition reads per liquidation pass.
	var iterErr error
	if err := k.accountKeeper.IterateAccountPositions(ctx, a.AccountIndex, func(pos accounttypes.AccountPosition) bool {
		if pos.Position.IsZero() {
			return false
		}
		marketIdx := pos.MarketIndex
		// Determine the relevant status (cross vs isolated). For
		// isolated, fetch the params directly so they can be reused
		// by the LLP / ADL callees below; for cross, reuse the
		// already-aggregated `crossRP`.
		var posStatus uint32
		var victimRP *risktypes.RiskParameters
		if pos.MarginMode == perptypes.IsolatedMargin {
			rp, err := k.riskKeeper.ComputeIsolatedRisk(ctx, a.AccountIndex, marketIdx)
			if err != nil {
				iterErr = err
				return true
			}
			isoRP := rp
			victimRP = &isoRP
			posStatus = riskkeeper.ClassifyHealth(rp)
		} else {
			posStatus = crossStatus
			victimRP = crossRP
		}

		if posStatus == perptypes.HealthHealthy || posStatus == perptypes.HealthPreLiquidation {
			_ = k.Flags.Remove(ctx, collections.Join(a.AccountIndex, marketIdx))
			return false
		}

		// Flag for keeper bots.
		flag := types.LiquidationFlag{
			AccountIndex:   a.AccountIndex,
			MarketIndex:    marketIdx,
			FlaggedAtBlock: height,
			FlaggedAtTime:  now,
		}
		if err := k.Flags.Set(ctx, collections.Join(a.AccountIndex, marketIdx), flag); err != nil {
			sdkCtx.Logger().Error("liquidation: set flag failed",
				"account", a.AccountIndex, "market", marketIdx, "err", err)
		}

		// FULL_LIQUIDATION + BANKRUPTCY: try the LLP first per the
		// Lighter spec ("LLP closes all of the user's positions by
		// taking them over"), gated by SimulateRiskAfterTakeover so
		// the LLP never breaches its IMR. Anything the LLP refuses
		// falls through to ADL.
		if attemptsLeft == nil || *attemptsLeft == 0 {
			return false
		}
		if posStatus == perptypes.HealthFullLiquidation || posStatus == perptypes.HealthBankruptcy {
			absorbed, err := k.tryLLPAbsorb(ctx, a.AccountIndex, marketIdx, attemptsLeft, victimRP)
			if err != nil {
				sdkCtx.Logger().Error("liquidation: LLP absorb failed",
					"victim", a.AccountIndex, "market", marketIdx, "err", err)
			}
			if !absorbed {
				if err := k.autoADL(ctx, a.AccountIndex, marketIdx, candCap, attemptsLeft, victimRP); err != nil {
					sdkCtx.Logger().Error("liquidation: auto-adl failed",
						"victim", a.AccountIndex, "market", marketIdx, "err", err)
				}
			}
		}
		// No silent IF top-up of residual negative collateral.
		// "Absorption" is the LLP/IF deleverage trade itself; if
		// `tryLLPAbsorb` rejected (IMR breach) and `autoADL` could not
		// find counterparties, the position simply remains and is re-
		// evaluated next block — Lighter's design has no analogue of
		// the previous `absorbNegativeCollateral` post-block sweep.
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
	if crossStatus != perptypes.HealthHealthy {
		sdkCtx.EventManager().EmitEvent(sdk.NewEvent(
			types.EventTypeLiquidationFlagged,
			sdk.NewAttribute(types.AttributeKeyAccountIndex, strconv.FormatUint(a.AccountIndex, 10)),
			sdk.NewAttribute(types.AttributeKeyStatus, strconv.FormatUint(uint64(crossStatus), 10)),
		))
	}
	return nil
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
		if err := k.Flags.Remove(ctx, key); err != nil {
			return err
		}
	}
	return nil
}
