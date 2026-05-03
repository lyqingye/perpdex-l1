package keeper

import (
	"context"
	"sort"

	"cosmossdk.io/math"

	perptypes "github.com/perpdex/perpdex-l1/types"
	accounttypes "github.com/perpdex/perpdex-l1/x/account/types"
)

// ADLCandidate is a counterparty considered for auto-deleveraging on a
// (market, side) pair. Candidates are ranked by `Score`, descending; the
// first entry is the most "ADL-able" (highest profit-to-collateral
// ratio).
type ADLCandidate struct {
	AccountIndex uint64
	// PositionSize is the candidate's signed perp position. It is
	// always opposite to the victim's side that produced this queue.
	PositionSize math.Int
	// UnrealizedPnL of the position at the current mark price.
	// Strictly positive — losing positions are filtered out.
	UnrealizedPnL math.Int
	// Score = uPnL * MARGIN_TICK / max(collateral, 1)
	//
	// This is the Hyperliquid-style "profit-to-collateral" metric: it
	// approximates `profit_ratio * leverage` after collapsing the
	// notionals (since both numerator and denominator contain notional
	// implicitly via uPnL and margin). Higher = closer to the front
	// of the ADL queue.
	Score math.Int
}

// BuildADLQueue scans every account, picks those that hold an opposing
// non-zero position in `marketIdx` AND are currently profitable on it,
// computes the ADL score and returns the top `limit` candidates sorted
// by score descending. `oppositeIsLong = true` means the victim is
// short, so the ADL queue must be longs (PositionSize > 0).
//
// Cost: O(N_accounts) per call. The caller is expected to apply the
// `MaxAdlCandidatesPerVictim` cap from Params before invoking this.
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
			a.AccountType == perptypes.PublicPoolAccountType ||
			a.AccountType == perptypes.InsuranceFundAccountType {
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
		uPnL, err := k.riskKeeper.GetPositionUnrealizedPnL(ctx, a.AccountIndex, marketIdx)
		if err != nil || !uPnL.IsPositive() {
			// Losing or unknown-PnL positions are not candidates.
			return false
		}
		collateral := a.Collateral
		if collateral.IsNil() || !collateral.IsPositive() {
			collateral = math.OneInt()
		}
		score := uPnL.Mul(math.NewIntFromUint64(uint64(perptypes.MarginTick))).Quo(collateral)
		out = append(out, ADLCandidate{
			AccountIndex:  a.AccountIndex,
			PositionSize:  pos.Position,
			UnrealizedPnL: uPnL,
			Score:         score,
		})
		return false
	}); err != nil {
		return nil, err
	}

	sort.Slice(out, func(i, j int) bool {
		// Primary: score desc. Tie-break: account_index asc for determinism.
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
