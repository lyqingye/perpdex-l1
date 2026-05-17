package keeper

import (
	"context"
	"sort"
	"strconv"

	"cosmossdk.io/math"
	sdk "github.com/cosmos/cosmos-sdk/types"

	perptypes "github.com/perpdex/perpdex-l1/types"
	accounttypes "github.com/perpdex/perpdex-l1/x/account/types"
	"github.com/perpdex/perpdex-l1/x/liquidation/types"
	risktypes "github.com/perpdex/perpdex-l1/x/risk/types"
	tradekeeper "github.com/perpdex/perpdex-l1/x/trade/keeper"
)

// ADLCandidate is a counterparty considered for ADL on (market, side).
// Ranked by Score desc — leverage and unrealized profit jointly, so
// highly-leveraged winners come first.
type ADLCandidate struct {
	AccountIndex uint64
	// Signed perp position; always opposite to the victim's side.
	PositionSize math.Int
	// uPnL at the current mark; strictly positive (losers filtered).
	UnrealizedPnL math.Int
	// Cached from the snapshot so autoADL can enforce zero-price
	// alignment without re-querying.
	ZeroPrice uint32
	// Cross-aggregate leverage at rank time (notional / max(coll, 1))
	// in MarginTick units. Always CROSS, even for isolated candidates.
	Leverage math.Int
	// Score = leverage * uPnL_ratio where
	// uPnL_ratio ≈ uPnL * MarginTick / max(|entry_quote|, 1).
	Score math.Int
}

// BuildADLQueue returns up to `limit` opposite-side, profitable
// counterparties in `marketIdx` ranked by ADL score desc.
// `oppositeIsLong=true` means the victim is short, so candidates must
// be longs.
//
// O(N_accounts) per call; the caller applies MaxAdlCandidatesPerVictim
// from Params. Each candidate is read through one
// GetLiquidationRiskSnapshot so (pos, mark, md, Risk, CrossRisk, ZP)
// stay internally consistent. Leverage always comes from
// snap.CrossRisk; isolated candidates are not filtered, the trade
// engine routes them via applyIsolatedAccount.
func (k Keeper) BuildADLQueue(
	ctx context.Context,
	marketIdx uint32,
	oppositeIsLong bool,
	limit uint32,
) ([]ADLCandidate, error) {
	if limit == 0 {
		return nil, nil
	}

	out := make([]ADLCandidate, 0, limit)
	if err := k.accountKeeper.IterateAccounts(ctx, func(a accounttypes.Account) bool {
		// Skip system accounts and pool sub-accounts: pool absorption
		// goes through EndBlocker's IF_FIRST routing, never the ranked
		// user queue.
		if a.AccountIndex == perptypes.TreasuryAccountIndex ||
			a.AccountIndex == perptypes.InsuranceFundOperatorAccountIdx ||
			a.IsPoolType() {
			return false
		}
		snap, err := k.riskKeeper.GetLiquidationRiskSnapshot(ctx, a.AccountIndex, marketIdx)
		if err != nil {
			return false
		}
		pos := snap.Position
		if pos.BaseSize.IsZero() {
			return false
		}
		// Opposite side only.
		if pos.IsLong() != oppositeIsLong {
			return false
		}
		uPnL := pos.UnrealizedPnL(snap.MarkPrice)
		if !uPnL.IsPositive() {
			return false
		}
		leverage := ComputeLeverage(snap.CrossRisk)
		entryAbs := pos.EntryQuote.Abs()
		if !entryAbs.IsPositive() {
			entryAbs = math.OneInt()
		}
		uPnLRatio := uPnL.Mul(math.NewInt(int64(perptypes.MarginTick))).Quo(entryAbs)
		score := leverage.Mul(uPnLRatio)
		out = append(out, ADLCandidate{
			AccountIndex:  a.AccountIndex,
			PositionSize:  pos.BaseSize,
			UnrealizedPnL: uPnL,
			ZeroPrice:     snap.ZeroPrice,
			Leverage:      leverage,
			Score:         score,
		})
		return false
	}); err != nil {
		return nil, err
	}

	sort.Slice(out, func(i, j int) bool {
		if !out[i].Score.Equal(out[j].Score) {
			return out[i].Score.GT(out[j].Score)
		}
		return out[i].AccountIndex < out[j].AccountIndex
	})

	if uint32(len(out)) > limit {
		out = out[:limit]
	}
	return out, nil
}

// ComputeLeverage returns IM * MarginTick / Collateral as the ADL
// ranking leverage proxy. Nil Collateral panics (risk-keeper
// invariant). Collateral <= 0 clamps to 1 (residual debt → ranks at
// front). IM == 0 returns neutral 1. Exported for unit tests; in-pkg
// callers only.
func ComputeLeverage(rp risktypes.RiskParameters) math.Int {
	if rp.Collateral.IsNil() {
		panic("liquidation: RiskParameters.Collateral is nil; upstream invariant violated")
	}
	if rp.InitialMarginRequirement.IsZero() {
		return math.OneInt()
	}
	collateral := rp.Collateral
	if !collateral.IsPositive() {
		collateral = math.OneInt()
	}
	return rp.InitialMarginRequirement.Mul(math.NewInt(int64(perptypes.MarginTick))).Quo(collateral)
}

// autoADL closes part of the victim's position in marketIdx against
// the top-ranked BuildADLQueue counterparties.
//
// Execution price must align zero prices on both sides — the midpoint
// (victimZP + candZP) / 2 satisfies both when zero prices overlap;
// non-overlapping pairs are skipped (counterparty would lose health).
//
// attemptsLeft is decremented per fill and shared across victims.
//
// The victim snapshot is rebuilt at entry AND after every fill: each
// fill mutates BaseSize / EntryQuote / Collateral and therefore TAV /
// MMR / ZP. Reusing entry-time state would feed stale data into the
// next overlap check / settle price; the refresh also gates "no ADL
// on a recovered account" intra-loop (the trade engine does not
// enforce victim health on the deleverage path).
func (k Keeper) autoADL(
	ctx context.Context,
	victim uint64,
	marketIdx uint32,
	candCap uint32,
	attemptsLeft *uint32,
) error {
	if attemptsLeft == nil || *attemptsLeft == 0 {
		return nil
	}
	snap, err := k.riskKeeper.GetLiquidationRiskSnapshot(ctx, victim, marketIdx)
	if err != nil {
		return err
	}
	pos := snap.Position
	if pos.BaseSize.IsZero() {
		return nil
	}
	if status := snap.Risk.HealthStatus(); status != perptypes.HealthFullLiquidation &&
		status != perptypes.HealthBankruptcy {
		// Victim recovered (sibling-market LLP fill earlier in the
		// block). ADL is reserved for FULL/BANKRUPTCY.
		return nil
	}
	victimZP := snap.ZeroPrice

	// Counterparties take the opposite side.
	oppositeIsLong := pos.IsShort()
	cands, err := k.BuildADLQueue(ctx, marketIdx, oppositeIsLong, candCap)
	if err != nil {
		return err
	}
	sdkCtx := sdk.UnwrapSDKContext(ctx)
	remaining := pos.BaseSize.Abs()
	takerIsAsk := pos.OpeningIsAsk()
	for _, c := range cands {
		if *attemptsLeft == 0 || remaining.IsZero() {
			break
		}
		// Zero-price overlap: non-overlapping pairs would push the
		// counterparty into liquidation.
		if oppositeIsLong {
			if victimZP < c.ZeroPrice {
				continue
			}
		} else {
			if victimZP > c.ZeroPrice {
				continue
			}
		}
		// Round midpoint to the victim-favourable side to remove the
		// 1-ulp floor bias.
		settlePrice := ZeroPriceMid(victimZP, c.ZeroPrice, !oppositeIsLong)
		size := c.PositionSize.Abs()
		if size.GT(remaining) {
			size = remaining
		}
		if !size.IsPositive() {
			continue
		}
		// autoADL settles at the midpoint, so it bypasses the
		// Deleverage wrapper and drives the trade engine directly.
		// No counterparty pre-check needed: size is bounded by the
		// candidate's position and the overlap guarantees settlePrice
		// is on the candidate-favourable side of its own ZP — closing
		// in that band can only improve the candidate's TAV/MMR.
		// IsValidRiskChangeFrom (taker side) is the backstop.
		if err := k.tradeKeeper.ApplyPerpsMatching(ctx, tradekeeper.PerpFill{
			MakerAccountIndex: victim,
			TakerAccountIndex: c.AccountIndex,
			MarketIndex:       marketIdx,
			Price:             settlePrice,
			BaseAmount:        size.Uint64(),
			IsTakerAsk:        takerIsAsk,
			NoFee:             true,
			// User-ADL: both sides go through IsValidRiskChangeFrom.
		}); err != nil {
			sdkCtx.Logger().Error("liquidation: auto-adl fill failed",
				"victim", victim, "market", marketIdx,
				"counterparty", c.AccountIndex, "err", err)
			continue
		}
		// EventTypeAutoADL carries the two zero prices;
		// EventTypeDeleverage is emitted alongside so indexers can
		// read every deleverage path from one stream tagged by source.
		sdkCtx.EventManager().EmitEvent(sdk.NewEvent(
			types.EventTypeAutoADL,
			sdk.NewAttribute(types.AttributeKeyVictim, strconv.FormatUint(victim, 10)),
			sdk.NewAttribute(types.AttributeKeyMarketIndex, strconv.FormatUint(uint64(marketIdx), 10)),
			sdk.NewAttribute(types.AttributeKeyCounterparty, strconv.FormatUint(c.AccountIndex, 10)),
			sdk.NewAttribute(types.AttributeKeyBaseAmount, strconv.FormatUint(size.Uint64(), 10)),
			sdk.NewAttribute(types.AttributeKeyPrice, strconv.FormatUint(uint64(settlePrice), 10)),
			sdk.NewAttribute(types.AttributeKeyVictimZeroPrice, strconv.FormatUint(uint64(victimZP), 10)),
			sdk.NewAttribute(types.AttributeKeyCandZeroPrice, strconv.FormatUint(uint64(c.ZeroPrice), 10)),
		))
		sdkCtx.EventManager().EmitEvent(sdk.NewEvent(
			types.EventTypeDeleverage,
			sdk.NewAttribute(types.AttributeKeyVictim, strconv.FormatUint(victim, 10)),
			sdk.NewAttribute(types.AttributeKeyDeleverager, strconv.FormatUint(c.AccountIndex, 10)),
			sdk.NewAttribute(types.AttributeKeyMarketIndex, strconv.FormatUint(uint64(marketIdx), 10)),
			sdk.NewAttribute(types.AttributeKeyBaseAmount, strconv.FormatUint(size.Uint64(), 10)),
			sdk.NewAttribute(types.AttributeKeyPrice, strconv.FormatUint(uint64(settlePrice), 10)),
			sdk.NewAttribute(types.AttributeKeySource, DeleverageSourceAutoADL),
		))
		*attemptsLeft--

		// Refresh: the fill shifted BaseSize / EntryQuote /
		// Collateral, so victimZP and remaining are now stale.
		// Subsequent overlap checks / settle prices MUST observe
		// the post-fill state; the loop must also short-circuit if
		// the victim closed out or recovered.
		snap, err = k.riskKeeper.GetLiquidationRiskSnapshot(ctx, victim, marketIdx)
		if err != nil {
			return err
		}
		if snap.Position.BaseSize.IsZero() {
			return nil
		}
		if status := snap.Risk.HealthStatus(); status != perptypes.HealthFullLiquidation &&
			status != perptypes.HealthBankruptcy {
			return nil
		}
		victimZP = snap.ZeroPrice
		remaining = snap.Position.BaseSize.Abs()
	}
	return nil
}

// ZeroPriceMid returns the integer midpoint of two zero prices,
// rounded victim-favourably (long → ceil, short → floor) to remove
// the 1-ulp bias plain floor division would compound across fills.
// Exported for unit tests; in-pkg callers only.
func ZeroPriceMid(a, b uint32, victimIsLong bool) uint32 {
	sum := uint64(a) + uint64(b)
	if victimIsLong {
		return uint32((sum + 1) / 2)
	}
	return uint32(sum / 2)
}
