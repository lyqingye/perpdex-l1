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

// ADLCandidate is a counterparty considered for auto-deleveraging on a
// (market, side) pair. Candidates are ranked by `Score`, descending; the
// first entry is the most "ADL-able" — Lighter spec ranks by leverage
// AND unrealized profit jointly so highly-leveraged winners get pulled
// in before low-leverage winners with the same uPnL.
type ADLCandidate struct {
	AccountIndex uint64
	// PositionSize is the candidate's signed perp position. It is
	// always opposite to the victim's side that produced this queue.
	PositionSize math.Int
	// UnrealizedPnL of the position at the current mark price.
	// Strictly positive — losing positions are filtered out.
	UnrealizedPnL math.Int
	// ZeroPrice cached from x/risk so autoADL can enforce zero-
	// price alignment without re-querying.
	ZeroPrice uint32
	// Leverage is the cross account leverage at rank time (notional /
	// max(collateral, 1)), expressed in MarginTick units.
	Leverage math.Int
	// Score = leverage * uPnL_ratio. uPnL_ratio is approximated by
	// uPnL * MarginTick / max(|entry_quote|, 1). Higher = closer to
	// the front of the ADL queue.
	Score math.Int
}

// BuildADLQueue scans every account, picks those that hold an opposing
// non-zero position in `marketIdx` AND are currently profitable on it,
// computes the per-Lighter ADL score and returns the top `limit`
// candidates sorted by score descending. `oppositeIsLong = true` means
// the victim is short, so the ADL queue must be longs (PositionSize > 0).
//
// Cost: O(N_accounts) per call. The caller is expected to apply the
// `MaxAdlCandidatesPerVictim` cap from Params before invoking this.
//
// Per-call prefetch: `mark` and `md` are fetched ONCE outside the
// per-candidate loop so each candidate only triggers (a) GetPosition,
// (b) ComputeRiskInfo / ComputeIsolatedRisk for leverage + ZP, and (c)
// the pure ZP math via `riskKeeper.ComputeZeroPrice`. Previously each
// candidate re-pulled the position three times, the mark price twice,
// market details once, and the risk aggregate twice.
func (k Keeper) BuildADLQueue(
	ctx context.Context,
	marketIdx uint32,
	oppositeIsLong bool,
	limit uint32,
) ([]ADLCandidate, error) {
	if limit == 0 {
		return nil, nil
	}
	mark, md, err := k.riskKeeper.GetMarkAndMarketDetails(ctx, marketIdx)
	if err != nil {
		return nil, err
	}

	out := make([]ADLCandidate, 0, limit)
	if err := k.accountKeeper.IterateAccounts(ctx, func(a accounttypes.Account) bool {
		// Skip system accounts (treasury, IF) and any other Public
		// Pool sub-accounts: pool absorption goes through the
		// IF_FIRST routing in EndBlocker, never the user-facing
		// ranked ADL queue.
		if a.AccountIndex == perptypes.TreasuryAccountIndex ||
			a.AccountIndex == perptypes.InsuranceFundOperatorAccountIdx ||
			a.IsPoolType() {
			return false
		}
		pos, err := k.accountKeeper.GetPosition(ctx, a.AccountIndex, marketIdx)
		if err != nil || pos.Position.IsZero() {
			return false
		}
		// Only opposite-side positions can offset a victim's close-out.
		if pos.Position.IsPositive() != oppositeIsLong {
			return false
		}
		uPnL := pos.UnrealizedPnL(mark)
		if !uPnL.IsPositive() {
			// Losing or zero-PnL positions are not candidates.
			return false
		}

		// Leverage + (TAV, MMR) for ZP both come from the same risk
		// scope; pick cross or isolated based on the position's
		// margin mode and reuse the result for both.
		var rp risktypes.RiskParameters
		if pos.MarginMode == perptypes.IsolatedMargin {
			ip, err := k.riskKeeper.ComputeIsolatedRisk(ctx, a.AccountIndex, marketIdx)
			if err != nil {
				return false
			}
			rp = ip
		} else {
			ri, err := k.riskKeeper.ComputeRiskInfo(ctx, a.AccountIndex)
			if err != nil {
				return false
			}
			if ri.CrossRiskParameters == nil {
				return false
			}
			rp = *ri.CrossRiskParameters
		}
		leverage := computeLeverage(rp)
		// uPnL_ratio = uPnL / max(|entry_quote|, 1).
		entryAbs := pos.EntryQuote.Abs()
		if !entryAbs.IsPositive() {
			entryAbs = math.OneInt()
		}
		uPnLRatio := uPnL.Mul(math.NewInt(int64(perptypes.MarginTick))).Quo(entryAbs)
		// Score = leverage * uPnL_ratio (in MarginTick^2 units).
		score := leverage.Mul(uPnLRatio)
		zp := k.riskKeeper.ComputeZeroPrice(pos, mark, md, rp.TotalAccountValue, rp.MaintenanceMarginRequirement)
		out = append(out, ADLCandidate{
			AccountIndex:  a.AccountIndex,
			PositionSize:  pos.Position,
			UnrealizedPnL: uPnL,
			ZeroPrice:     zp,
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

// computeLeverage approximates an account's leverage from its risk
// parameters. We use IM as a proxy for notional (notional = IM *
// MarginTick / IMF), so the ratio collapses to IM/Collateral scaled by
// MarginTick. Both numerator and denominator scale by the same
// constant for the same account — fine for ranking purposes.
func computeLeverage(rp risktypes.RiskParameters) math.Int {
	collateral := rp.Collateral
	if collateral.IsNil() || !collateral.IsPositive() {
		collateral = math.OneInt()
	}
	if rp.InitialMarginRequirement.IsZero() {
		return math.OneInt()
	}
	return rp.InitialMarginRequirement.Mul(math.NewInt(int64(perptypes.MarginTick))).Quo(collateral)
}

// autoADL closes a portion of the victim's `marketIdx` position against
// the top-ranked counterparties returned by BuildADLQueue.
//
// Per the Lighter spec the trade between the bankrupt account and an
// opposite-side counterparty MUST happen at a price where the two
// "zero prices" align — i.e. the execution price is at least as good
// as either side's zero price. The exact midpoint
// `(victimZP + candZP) / 2` satisfies both invariants when the prices
// overlap; pairs whose zero prices do NOT overlap are skipped (the
// counterparty would lose health).
//
// `attemptsLeft` is decremented per successful fill and shared across
// all victims in the block.
//
// `victimRP` lets the EndBlocker hand in the cross / isolated risk
// parameters it already fetched for `victim`, avoiding a redundant
// ComputeRiskInfo / ComputeIsolatedRisk inside the ZP computation.
// Pass nil when the caller has no cached state (msg-server entry, etc.).
func (k Keeper) autoADL(
	ctx context.Context,
	victim uint64,
	marketIdx uint32,
	candCap uint32,
	attemptsLeft *uint32,
	victimRP *risktypes.RiskParameters,
) error {
	if attemptsLeft == nil || *attemptsLeft == 0 {
		return nil
	}
	pos, err := k.accountKeeper.GetPosition(ctx, victim, marketIdx)
	if err != nil {
		return err
	}
	if pos.Position.IsZero() {
		return nil
	}
	mark, md, err := k.riskKeeper.GetMarkAndMarketDetails(ctx, marketIdx)
	if err != nil {
		return err
	}
	rp, err := k.resolveVictimRiskParams(ctx, victim, marketIdx, pos, victimRP)
	if err != nil {
		return err
	}
	victimZP := k.riskKeeper.ComputeZeroPrice(pos, mark, md, rp.TotalAccountValue, rp.MaintenanceMarginRequirement)

	// Victim long  → counterparties must be short to offset.
	// Victim short → counterparties must be long.
	oppositeIsLong := pos.Position.IsNegative()
	cands, err := k.BuildADLQueue(ctx, marketIdx, oppositeIsLong, candCap)
	if err != nil {
		return err
	}
	sdkCtx := sdk.UnwrapSDKContext(ctx)
	remaining := pos.Position.Abs()
	takerIsAsk := pos.Position.IsNegative()
	for _, c := range cands {
		if *attemptsLeft == 0 || remaining.IsZero() {
			break
		}
		// Zero-price alignment check. For a victim-long ADL the
		// victim's zero price is BELOW mark and the cand's (short)
		// zero price is ABOVE mark — overlap requires victimZP <=
		// candZP. The mirror inequality applies for victim-short.
		if oppositeIsLong {
			// Victim short → cand long. Victim's ZP is above mark,
			// cand's is below mark. Settlement requires victimZP
			// >= candZP so the price band exists.
			if victimZP < c.ZeroPrice {
				continue
			}
		} else {
			// Victim long → cand short. Symmetric: victimZP <=
			// candZP.
			if victimZP > c.ZeroPrice {
				continue
			}
		}
		settlePrice := zeroPriceMid(victimZP, c.ZeroPrice)
		size := c.PositionSize.Abs()
		if size.GT(remaining) {
			size = remaining
		}
		if !size.IsPositive() {
			continue
		}
		fill := tradekeeper.PerpFill{
			MakerAccountIndex: victim,
			TakerAccountIndex: c.AccountIndex,
			MarketIndex:       marketIdx,
			Price:             settlePrice,
			BaseAmount:        size.Uint64(),
			IsTakerAsk:        takerIsAsk,
			NoFee:             true,
			// User-ADL: defense-in-depth — both bankrupt (maker)
			// and counterparty (taker) go through
			// IsValidRiskChange. The bankrupt check mirrors
			// Lighter's `is_valid_risk_change` on bankrupt; the
			// counterparty check is perpdex-stricter than Lighter
			// (which does only a collateral-sufficiency assert on
			// ADL deleveragers). The settlement at zeroPriceMid
			// guarantees the counterparty's TAV/MMR cannot
			// regress, so the check passes in normal flow but
			// still catches pathological pricing. Both flags
			// default to false here because we DO want both risk
			// checks under user-ADL.
		}
		// Pre-trade collateral assert on the counterparty side only
		// (mirrors the guard inside Deleverage's user-ADL branch).
		// autoADL fills go through the engine directly because the
		// settle price differs from the victim's zero price
		// (`zeroPriceMid` covers the overlap of both sides' zero
		// prices), so we can't reuse Deleverage as a wrapper.
		// Replicating the assert keeps both deleverage codepaths
		// consistently funding-aware. The bankrupt side is not
		// asserted — see Deleverage docstring for rationale.
		if err := k.preCheckCollateral(
			ctx, c.AccountIndex, marketIdx, size.Uint64(), settlePrice,
			true /*isTakerSide*/, takerIsAsk, "counterparty",
		); err != nil {
			sdkCtx.Logger().Info("liquidation: auto-adl skipped (insufficient counterparty collateral)",
				"victim", victim, "market", marketIdx,
				"counterparty", c.AccountIndex, "err", err)
			continue
		}
		if err := k.tradeKeeper.ApplyPerpsMatching(ctx, fill); err != nil {
			sdkCtx.Logger().Error("liquidation: auto-adl fill failed",
				"victim", victim, "market", marketIdx,
				"counterparty", c.AccountIndex, "err", err)
			continue
		}
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
		remaining = remaining.Sub(size)
		*attemptsLeft--
	}
	return nil
}

// resolveVictimRiskParams returns the cross or isolated RiskParameters
// the ZP computation needs. When `cached` is non-nil it's used
// verbatim, sparing a redundant ComputeRiskInfo / ComputeIsolatedRisk
// call. Centralised so autoADL / tryLLPAbsorb / Liquidate / Deleverage
// don't each re-implement the cross-vs-isolated branch.
func (k Keeper) resolveVictimRiskParams(
	ctx context.Context,
	victim uint64,
	marketIdx uint32,
	pos accounttypes.AccountPosition,
	cached *risktypes.RiskParameters,
) (risktypes.RiskParameters, error) {
	if cached != nil {
		return *cached, nil
	}
	if pos.MarginMode == perptypes.IsolatedMargin {
		return k.riskKeeper.ComputeIsolatedRisk(ctx, victim, marketIdx)
	}
	ri, err := k.riskKeeper.ComputeRiskInfo(ctx, victim)
	if err != nil {
		return risktypes.RiskParameters{}, err
	}
	if ri.CrossRiskParameters == nil {
		return risktypes.RiskParameters{}, nil
	}
	return *ri.CrossRiskParameters, nil
}

// zeroPriceMid returns the integer midpoint of two zero prices. Both
// arguments are uint32 prices; the midpoint never overflows uint32.
func zeroPriceMid(a, b uint32) uint32 {
	// (a + b) / 2 with uint64 widening to avoid wrap-around even in
	// the (theoretical) MaxOrderPrice + MaxOrderPrice case.
	return uint32((uint64(a) + uint64(b)) / 2)
}
