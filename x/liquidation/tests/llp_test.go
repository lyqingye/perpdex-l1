// Liquidation Liquidity Pool (LLP / Insurance Fund) takeover gating.
//
// The LLP path is the first stop in the bankrupt-resolution waterfall:
// the EndBlocker offers the worst-uPnL victim position to the LLP
// (Insurance Fund operator) before falling through to ADL. Before any
// fill is issued, `tryLLPAbsorb` simulates the post-takeover risk and
// MUST refuse the absorption if it would leave the LLP itself below
// its own IMR. This file pins the refusal-and-fallthrough invariant.
package tests

import (
	"testing"

	"cosmossdk.io/math"

	"github.com/stretchr/testify/require"

	perptypes "github.com/perpdex/perpdex-l1/types"
	accounttypes "github.com/perpdex/perpdex-l1/x/account/types"
	risktypes "github.com/perpdex/perpdex-l1/x/risk/types"
)

// TestLLPAbsorb_StopsWhenLLPWouldBreachIMR verifies that the LLP IMR
// safety gate blocks takeover when SimulateRiskAfterTakeover reports
// post.TAV < post.IMR; the position falls through to ADL instead.
func TestLLPAbsorb_StopsWhenLLPWouldBreachIMR(t *testing.T) {
	ak := newStubAccount()
	ak.accounts[perptypes.InsuranceFundOperatorAccountIdx] = accounttypes.Account{
		AccountIndex: perptypes.InsuranceFundOperatorAccountIdx,
		AccountType:  perptypes.InsuranceFundAccountType,
		Collateral:   math.NewInt(100), // tiny; can't absorb
		PublicPoolInfo: &accounttypes.PublicPoolInfo{
			Status:         perptypes.PublicPoolStatusActive,
			TotalShares:    math.NewInt(1),
			OperatorShares: math.NewInt(1),
		},
	}
	// One ADL counterparty (account 999) on the opposite side, in
	// profit, so autoADL has someone to fill against.
	ak.accounts[999] = accounttypes.Account{
		AccountIndex: 999,
		AccountType:  perptypes.MasterAccountType,
		Collateral:   math.NewInt(1_000_000),
	}
	ak.pos[[2]uint64{999, 0}] = accounttypes.AccountPosition{
		AccountIndex: 999, MarketIndex: 0,
		BaseSize: math.NewInt(-10), EntryQuote: math.NewInt(-2_000),
		LastFundingRatePrefixSum: math.ZeroInt(), AllocatedMargin: math.ZeroInt(),
	}
	// Bankrupt has a small but non-zero cushion so the autoADL pre-
	// trade collateral assert (`is_bankrupt_has_enough_cross_
	// collateral`) passes at the candidate's settle price (mid of
	// 100/110 = 105). The realised PnL on a 10-unit close at 105
	// against EQ=+5000 is small (~50), so 100 of cushion is plenty.
	ak.accounts[100] = accounttypes.Account{AccountIndex: 100, Collateral: math.NewInt(100)}
	ak.pos[[2]uint64{100, 0}] = accounttypes.AccountPosition{
		AccountIndex: 100, MarketIndex: 0,
		BaseSize: math.NewInt(50), EntryQuote: math.NewInt(5_000),
		LastFundingRatePrefixSum: math.ZeroInt(), AllocatedMargin: math.ZeroInt(),
	}
	rk := newStubRisk()
	rk.status = perptypes.HealthFullLiquidation
	// At default markPrice=100: victim 100 (50, 5000) → uPnL=-4500 (worst).
	// Cand 999 (-10, -2000) → uPnL=1000 (>0, qualifies as ADL cand).
	rk.zero[[2]uint64{100, 0}] = 100
	rk.zero[[2]uint64{999, 0}] = 110
	// LLP would breach IMR: simulate post-state with TAV < IMR.
	rk.postSim[perptypes.InsuranceFundOperatorAccountIdx] = risktypes.RiskParameters{
		Collateral:                   math.NewInt(100),
		TotalAccountValue:            math.NewInt(50),
		InitialMarginRequirement:     math.NewInt(500), // breaches
		MaintenanceMarginRequirement: math.NewInt(250),
		CloseOutMarginRequirement:    math.NewInt(125),
	}
	tk := &stubTrade{}
	matchk := newStubMatching()
	k, ctx := newKeeper(t, ak, rk, tk, matchk)

	require.NoError(t, k.EndBlocker(ctx))

	// No LLP fill should be issued; instead an ADL fill (taker = 999).
	require.NotEmpty(t, tk.calls, "ADL must run when LLP refuses")
	for _, f := range tk.calls {
		require.NotEqual(t, perptypes.InsuranceFundOperatorAccountIdx, f.TakerAccountIndex,
			"LLP must not be taker when IMR check fails")
	}
}
