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
// first entry is the most "ADL-able" — the spec ranks by leverage AND
// unrealized profit jointly so highly-leveraged winners get pulled in
// before low-leverage winners with the same uPnL.
type ADLCandidate struct {
	AccountIndex uint64
	// PositionSize is the candidate's signed perp position. It is
	// always opposite to the victim's side that produced this queue.
	PositionSize math.Int
	// UnrealizedPnL of the position at the current mark price.
	// Strictly positive — losing positions are filtered out.
	UnrealizedPnL math.Int
	// ZeroPrice cached from the snapshot so autoADL can enforce
	// zero-price alignment without re-querying.
	ZeroPrice uint32
	// Leverage is the cross account leverage at rank time (notional /
	// max(collateral, 1)), expressed in MarginTick units. Always the
	// CROSS aggregate, even for isolated candidates, per the spec
	// ("highly-leveraged winners come first").
	Leverage math.Int
	// Score = leverage * uPnL_ratio. uPnL_ratio is approximated by
	// uPnL * MarginTick / max(|entry_quote|, 1). Higher = closer to
	// the front of the ADL queue.
	Score math.Int
}

// BuildADLQueue scans every account, picks those that hold an opposing
// non-zero position in `marketIdx` AND are currently profitable on it,
// computes the ADL score, and returns the top `limit` candidates
// sorted by score descending. `oppositeIsLong = true` means the victim
// is short, so the ADL queue must be longs (PositionSize > 0).
//
// Cost: O(N_accounts) per call. The caller is expected to apply the
// `MaxAdlCandidatesPerVictim` cap from Params before invoking this.
//
// Each candidate is read through one `GetLiquidationRiskSnapshot` call
// so the (pos, mark, md, Risk, CrossRisk, ZeroPrice) bundle stays
// internally consistent — uPnL is computed from the same mark the
// snapshot's ZeroPrice was anchored to. Ranking always uses
// `snap.CrossRisk` even when the candidate's targeted position is
// isolated, so the cross aggregate drives leverage in both modes.
//
// Isolated candidates are not filtered out; the trade engine's
// per-side `applyAccount` dispatcher routes their fill through
// `applyIsolatedAccount` (instead of `applyCrossAccount`), so
// isolated envelopes settle against their own `AllocatedMargin`
// without touching cross collateral.
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
		// Skip system accounts (treasury, IF) and any other Public
		// Pool sub-accounts: pool absorption goes through the
		// IF_FIRST routing in EndBlocker, never the user-facing
		// ranked ADL queue.
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
		// Only opposite-side positions can offset a victim's close-out.
		if pos.IsLong() != oppositeIsLong {
			return false
		}
		uPnL := pos.UnrealizedPnL(snap.MarkPrice)
		if !uPnL.IsPositive() {
			return false
		}
		leverage := ComputeLeverage(snap.CrossRisk)
		// uPnL_ratio = uPnL / max(|entry_quote|, 1).
		entryAbs := pos.EntryQuote.Abs()
		if !entryAbs.IsPositive() {
			entryAbs = math.OneInt()
		}
		uPnLRatio := uPnL.Mul(math.NewInt(int64(perptypes.MarginTick))).Quo(entryAbs)
		// Score = leverage * uPnL_ratio (in MarginTick^2 units).
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

// ComputeLeverage returns `IM * MarginTick / Collateral` as a leverage
// proxy used only for ADL ranking. `Collateral.IsNil()` is a risk
// keeper invariant violation and panics. `Collateral <= 0` (residual
// debt, fully wiped account) clamps to 1 so the candidate ranks at
// the front of the queue. `IM == 0` returns a neutral 1.
//
// Exported solely so the external `tests/` package can unit-test the
// edge cases; production callers all live in this package.
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

// autoADL closes a portion of the victim's `marketIdx` position
// against the top-ranked counterparties returned by BuildADLQueue.
//
// Per the spec the trade between the bankrupt account and an
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
// The victim's snapshot is rebuilt INSIDE this call so victim TAV/MMR
// reflect any state mutation that happened earlier in the same
// EndBlocker iteration. autoADL also self-asserts that the victim is
// still FULL_LIQUIDATION / BANKRUPTCY against that fresh snapshot —
// the trade engine does not enforce victim health on the deleverage
// path, so this routine is the canonical "no ADL on a recovered
// account" gate.
//
// The victim snapshot is ALSO re-read after every successful fill in
// the loop below. Each fill mutates the victim's BaseSize / EntryQuote
// / Collateral, which shifts both TAV and MMR and therefore the
// zero price; reusing the entry-time `victimZP` for subsequent
// overlap checks and settle prices would feed stale state into the
// next iteration and let `IsValidRiskChangeFrom` reject fills that a
// fresh ZP would have routed away. The refresh also lets the loop
// exit early when an earlier fill already restored the victim to
// HEALTHY / PRE / PARTIAL, preserving the "no ADL on a recovered
// account" gate intra-loop.
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
		// Victim recovered (e.g., a sibling market's LLP fill
		// earlier in this block). ADL is reserved for FULL /
		// BANKRUPTCY victims; refuse the fill.
		return nil
	}
	victimZP := snap.ZeroPrice

	// Victim long  → counterparties must be short to offset.
	// Victim short → counterparties must be long.
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
		// Zero-price overlap: victim long → cand short needs
		// victimZP <= candZP; victim short → cand long needs
		// victimZP >= candZP. Non-overlapping pairs would push the
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
		// Round the midpoint toward the victim-favourable side
		// (long victim → ceil, short victim → floor) to remove the
		// 1-ulp floor bias.
		settlePrice := ZeroPriceMid(victimZP, c.ZeroPrice, !oppositeIsLong)
		size := c.PositionSize.Abs()
		if size.GT(remaining) {
			size = remaining
		}
		if !size.IsPositive() {
			continue
		}
		// autoADL settles at the midpoint, not at the victim's zero
		// price, so the fill cannot reuse `Deleverage` as a wrapper
		// — drive the trade engine directly. No counterparty-side
		// `preCheckCollateral` is run here because the price-by-
		// construction guarantees the fill cannot worsen a
		// deleverager's health:
		//
		//  1. `size` is bounded by the deleverager's actual
		//     position above (`size = min(c.PositionSize.Abs(),
		//     remaining)`); no external caller can over-size the
		//     trade.
		//  2. `settlePrice = ZeroPriceMid(victimZP, candZP)` lies
		//     between the two zero prices; the overlap check
		//     above (`victimZP {<=,>=} candZP`) guarantees
		//     `settlePrice` is on the "better than candZP" side
		//     for the deleverager — closing in that band can only
		//     improve the deleverager's TAV/MMR ratio relative to
		//     trading at its own zero price (which is, by
		//     definition, the price that leaves the ratio
		//     invariant).
		//
		// The post-trade `IsValidRiskChangeFrom` run inside
		// `ApplyPerpsMatching` (deleverager taker side, no
		// `SkipTakerRiskCheck`) remains as defense-in-depth and
		// uses the full TAV-aware `ComputeCrossRisk` aggregate —
		// strictly stronger than the cash-only `Collateral` field
		// that the previous `preCheckCollateral` consulted. So the
		// removal both fixes the F6 false-skip (cross collateral
		// excludes other-market uPnL even when the account is
		// healthy) and removes a redundant pre-filter.
		if err := k.tradeKeeper.ApplyPerpsMatching(ctx, tradekeeper.PerpFill{
			MakerAccountIndex: victim,
			TakerAccountIndex: c.AccountIndex,
			MarketIndex:       marketIdx,
			Price:             settlePrice,
			BaseAmount:        size.Uint64(),
			IsTakerAsk:        takerIsAsk,
			NoFee:             true,
			// User-ADL: both bankrupt (maker) and counterparty
			// (taker) go through IsValidRiskChangeFrom.
		}); err != nil {
			sdkCtx.Logger().Error("liquidation: auto-adl fill failed",
				"victim", victim, "market", marketIdx,
				"counterparty", c.AccountIndex, "err", err)
			continue
		}
		// EventTypeAutoADL carries ADL-specific context (the two
		// zero prices); EventTypeDeleverage is emitted alongside it
		// so downstream indexers can read every deleverage path
		// from a single event stream tagged by `source`.
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

		// Refresh the victim snapshot. The fill we just emitted
		// shifted the victim's BaseSize / EntryQuote / Collateral,
		// so the entry-time `victimZP` and locally-decremented
		// `remaining` are now stale. Subsequent overlap checks and
		// `ZeroPriceMid` settle prices MUST observe the post-fill
		// state, and the loop must short-circuit if the victim has
		// already been closed out or recovered above the ADL
		// envelope.
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
// rounded toward the victim-favourable side (long victim → ceil,
// short victim → floor) to remove the 1-ulp bias plain floor division
// would compound across many ADL fills.
//
// Exported solely so the external `tests/` package can unit-test the
// rounding edges; production callers all live in this package.
func ZeroPriceMid(a, b uint32, victimIsLong bool) uint32 {
	sum := uint64(a) + uint64(b)
	if victimIsLong {
		return uint32((sum + 1) / 2)
	}
	return uint32(sum / 2)
}
