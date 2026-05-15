package keeper

import (
	"context"
	"errors"
	"sort"
	"strconv"

	"cosmossdk.io/math"
	sdk "github.com/cosmos/cosmos-sdk/types"

	perptypes "github.com/perpdex/perpdex-l1/types"
	accounttypes "github.com/perpdex/perpdex-l1/x/account/types"
	"github.com/perpdex/perpdex-l1/x/liquidation/types"
	risktypes "github.com/perpdex/perpdex-l1/x/risk/types"
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
// snapshot's ZeroPrice was anchored to. ADL ranking deliberately uses
// `snap.CrossRisk` even when the candidate's targeted position is
// isolated.
//
// Isolated positions are NOT filtered out. ADL operates symmetrically
// on cross and isolated counterparties:
//
//   - Ranking always uses the candidate's CROSS aggregate leverage
//     (`snap.CrossRisk`), so a high-leverage cross account with an
//     isolated winner still ranks via its cross leverage. This keeps
//     the ranking signal consistent with how the spec describes the
//     ADL queue ("rank by leverage AND profit").
//   - Execution settles via `Deleverage` which, on the deleverager
//     side, routes through `preCheckCollateral` (see
//     [liquidate.go](liquidate.go)). That helper splits on the
//     position's MarginMode: isolated candidates have their cushion
//     checked against `pos.AllocatedMargin` (the isolated envelope),
//     cross candidates against `account.Collateral`.
//
// Net effect: an isolated counterparty can absorb ADL flow as long as
// its allocated margin can swallow the realized loss; it does NOT
// pull collateral from the candidate's cross account, and a cross
// account's other isolated envelopes are not touched. There is no
// special-case path — both modes share the same Deleverage entry.
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
		leverage := computeLeverage(snap.CrossRisk)
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

// computeLeverage approximates an account's leverage from its risk
// parameters. We use IM as a proxy for notional (notional = IM *
// MarginTick / IMF), so the ratio collapses to IM/Collateral scaled by
// MarginTick. Both numerator and denominator scale by the same
// constant for the same account — fine for ranking purposes.
//
// Edge cases:
//
//   - `rp.Collateral.IsNil()` is an invariant violation by the risk
//     keeper: `ComputeCrossRisk` / `ComputeIsolatedRisk` always
//     populate Collateral from `account.Collateral` (which is never
//     nil for an existing account). Reaching this branch means an
//     uninitialised `RiskParameters{}` propagated through —
//     deliberately panic so the upstream bug surfaces immediately
//     instead of silently degrading ADL rank to leverage=1.
//   - `rp.Collateral <= 0` is a legitimate-but-extreme state (the
//     account's USDC cash has been wiped, e.g. residual debt). We
//     clamp Collateral to 1 so the ratio collapses to `IM * MarginTick`
//     — i.e. the candidate ranks at the front of the ADL queue,
//     which is the intended ordering for "no cushion left".
//   - `rp.InitialMarginRequirement.IsZero()` means the account has
//     no open positions whose IM contributes to cross — leverage is
//     not meaningful, return 1 so the score multiplier is neutral.
func computeLeverage(rp risktypes.RiskParameters) math.Int {
	if rp.Collateral.IsNil() {
		panic("liquidation: computeLeverage saw RiskParameters.Collateral == nil; upstream risk keeper invariant violated")
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
		// `oppositeIsLong` is the cand's side, so victim is its
		// opposite. Pass victim's direction into the midpoint
		// rounding so floor truncation tilts toward the side that
		// keeps victim's TAV intact (long victim → ceiling; short
		// victim → floor).
		settlePrice := zeroPriceMid(victimZP, c.ZeroPrice, !oppositeIsLong)
		size := c.PositionSize.Abs()
		if size.GT(remaining) {
			size = remaining
		}
		if !size.IsPositive() {
			continue
		}
		// Drive the fill through `Deleverage` with a settle-price
		// override so MsgDeleverage and autoADL share one
		// preCheckCollateral / risk-check / event emission path.
		// `ErrInsufficientCollateral` is the documented
		// "counterparty cannot absorb this fill" signal — we treat
		// it as a graceful skip and advance to the next candidate,
		// matching the pre-collapse autoADL behaviour. Other
		// errors are logged and skipped (preserving the
		// "EndBlocker keeps making progress" contract).
		if err := k.Deleverage(
			ctx, victim, marketIdx, c.AccountIndex, size.Uint64(),
			WithSettlePrice(settlePrice),
			WithDeleverageSource(DeleverageSourceAutoADL),
		); err != nil {
			if errors.Is(err, types.ErrInsufficientCollateral) {
				sdkCtx.Logger().Info("liquidation: auto-adl skipped (insufficient counterparty collateral)",
					"victim", victim, "market", marketIdx,
					"counterparty", c.AccountIndex, "err", err)
				continue
			}
			sdkCtx.Logger().Error("liquidation: auto-adl fill failed",
				"victim", victim, "market", marketIdx,
				"counterparty", c.AccountIndex, "err", err)
			continue
		}
		// `EventTypeAutoADL` is the ADL-specific audit event (it
		// carries victim/cand zero prices that downstream indexers
		// use for spread analysis). `EventTypeDeleverage` is also
		// emitted inside `Deleverage` with `source=auto_adl`, so a
		// single deleverage stream is sufficient for callers that
		// only care about the unified entry-point view.
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

// zeroPriceMid returns the integer midpoint of two zero prices,
// rounded toward the side that protects the victim's TAV. Integer
// floor division on its own systematically tilts the midpoint by up to
// 1 ulp; across many ADL fills the bias compounds against the victim.
// We instead round based on the victim's direction:
//
//   - victim long  → settle price closer to the higher endpoint is
//     more favourable to victim, so we round UP (ceiling).
//   - victim short → settle price closer to the lower endpoint is
//     more favourable to victim, so we round DOWN (floor).
//
// Both arguments are uint32 prices; the midpoint never overflows
// uint32 (uint64 widening guards even MaxOrderPrice + MaxOrderPrice).
func zeroPriceMid(a, b uint32, victimIsLong bool) uint32 {
	sum := uint64(a) + uint64(b)
	if victimIsLong {
		return uint32((sum + 1) / 2)
	}
	return uint32(sum / 2)
}
