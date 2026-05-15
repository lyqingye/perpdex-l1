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
// Isolated candidates are not filtered out; on execution
// `preCheckCollateral` swaps the cushion source to the position's
// `AllocatedMargin`, so isolated envelopes settle against their own
// margin without touching cross collateral.
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
		// — drive the trade engine directly. The preCheckCollateral
		// guard is replicated here for the deleverager (counterparty)
		// only, mirroring Deleverage's user-side branch.
		if err := k.preCheckCollateral(
			ctx, c.AccountIndex, marketIdx, size.Uint64(), settlePrice,
			true /*isTakerSide*/, takerIsAsk, "counterparty",
		); err != nil {
			sdkCtx.Logger().Info("liquidation: auto-adl skipped (insufficient counterparty collateral)",
				"victim", victim, "market", marketIdx,
				"counterparty", c.AccountIndex, "err", err)
			continue
		}
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
		remaining = remaining.Sub(size)
		*attemptsLeft--
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
