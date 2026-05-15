// EndBlocker scheduling for x/liquidation.
//
// The EndBlocker drives the LLP→ADL waterfall and the cross-aggregate
// refresh that protects multi-market accounts from stale snapshots.
// This file covers:
//
//   - Happy-path FULL_LIQUIDATION: LLP absorbs the worst-uPnL position
//     first; no ADL fill is generated when the LLP accepts.
//   - Bankruptcy fall-through to ADL when the LLP refuses on IMR.
//   - PRE_LIQUIDATION/healthy short-circuit: no fills.
//   - Bankrupt-residue retention when LLP refuses AND the ADL queue is
//     empty (no silent IF top-up).
//   - ADL candidate-skip: an under-collateralised first candidate must
//     not stop the loop; the next candidate takes over.
//   - Cross-aggregate freshness across markets: a mid-iteration heal
//     must be observed before the next market's trigger fires.
package tests

import (
	"testing"

	"cosmossdk.io/math"

	"github.com/stretchr/testify/require"

	perptypes "github.com/perpdex/perpdex-l1/types"
	accounttypes "github.com/perpdex/perpdex-l1/x/account/types"
	risktypes "github.com/perpdex/perpdex-l1/x/risk/types"
	tradekeeper "github.com/perpdex/perpdex-l1/x/trade/keeper"
)

// TestEndBlocker_FullLiquidationPrefersLLPThenADL exercises the
// FULL_LIQUIDATION branch: the LLP (IF) is offered the worst-uPnL
// position first; if it accepts, no ADL fill is generated.
func TestEndBlocker_FullLiquidationPrefersLLPThenADL(t *testing.T) {
	ak := newStubAccount()
	// Insurance Fund pool (idx 1, ACTIVE).
	ak.accounts[perptypes.InsuranceFundOperatorAccountIdx] = accounttypes.Account{
		AccountIndex: perptypes.InsuranceFundOperatorAccountIdx,
		AccountType:  perptypes.InsuranceFundAccountType,
		Collateral:   math.NewInt(10_000_000),
		PublicPoolInfo: &accounttypes.PublicPoolInfo{
			Status:         perptypes.PublicPoolStatusActive,
			TotalShares:    math.NewInt(1),
			OperatorShares: math.NewInt(1),
		},
	}
	// Victim with one FULL_LIQUIDATION cross position. EntryQuote
	// follows the production canonical sign (long → positive
	// notional in) so the pre-trade collateral assert can pass —
	// closing this position at zeroPrice=10 yields a realised PnL
	// of +4_500 in the engine's "Collateral += RealizedPnL" frame.
	ak.accounts[100] = accounttypes.Account{AccountIndex: 100, Collateral: math.NewInt(10)}
	ak.pos[[2]uint64{100, 0}] = accounttypes.AccountPosition{
		AccountIndex: 100, MarketIndex: 0,
		BaseSize: math.NewInt(50), EntryQuote: math.NewInt(5_000),
		LastFundingRatePrefixSum: math.ZeroInt(), AllocatedMargin: math.ZeroInt(),
	}
	rk := newStubRisk()
	rk.status = perptypes.HealthFullLiquidation
	// uPnL ordering is derived from `pos.UnrealizedPnL(markPrice)`; victim
	// 100 holds (Position=50, EntryQuote=5000), so at the stub default
	// markPrice=100 the uPnL is -4500 (loss). Single position → trivially
	// the worst.
	tk := &stubTrade{}
	matchk := newStubMatching()
	k, ctx := newKeeper(t, ak, rk, tk, matchk)

	require.NoError(t, k.EndBlocker(ctx))

	// Exactly one fill, target = IF as taker.
	require.Len(t, tk.calls, 1, "LLP absorb should produce one fill")
	require.Equal(t, perptypes.InsuranceFundOperatorAccountIdx, tk.calls[0].TakerAccountIndex,
		"counterparty must be the insurance fund operator")
	require.True(t, tk.calls[0].NoFee, "LLP takeover is a fee-less close")
	require.True(t, tk.calls[0].SkipTakerRiskCheck,
		"LLP / IF deleverager bypasses post-trade taker risk check")
	require.False(t, tk.calls[0].SkipMakerRiskCheck,
		"bankrupt (maker) post-trade risk check must remain enabled")
}

// TestEndBlocker_BankruptcyFallsThroughToADLWhenLLPBreachesIMR
// verifies the rule: a deeply bankrupt account whose absorption
// would breach the LLP's IMR is closed via ADL instead, leaving the
// LLP untouched.
func TestEndBlocker_BankruptcyFallsThroughToADLWhenLLPBreachesIMR(t *testing.T) {
	ak := newStubAccount()
	ak.accounts[perptypes.InsuranceFundOperatorAccountIdx] = accounttypes.Account{
		AccountIndex: perptypes.InsuranceFundOperatorAccountIdx,
		AccountType:  perptypes.InsuranceFundAccountType,
		Collateral:   math.NewInt(100), // tiny; absorbing this position breaches IMR
		PublicPoolInfo: &accounttypes.PublicPoolInfo{
			Status: perptypes.PublicPoolStatusActive,
		},
	}
	ak.accounts[999] = accounttypes.Account{
		AccountIndex: 999, AccountType: perptypes.MasterAccountType,
		Collateral: math.NewInt(1_000_000),
	}
	ak.pos[[2]uint64{999, 0}] = accounttypes.AccountPosition{
		AccountIndex: 999, MarketIndex: 0,
		BaseSize: math.NewInt(-10), EntryQuote: math.NewInt(-2_000),
		LastFundingRatePrefixSum: math.ZeroInt(), AllocatedMargin: math.ZeroInt(),
	}
	// Victim is BANKRUPT but the trade-mechanical realised PnL still
	// has to fit available collateral; the test gives the bankrupt a
	// modest cushion (300) so the pre-trade collateral assert in
	// autoADL can pass at the candidate's settle price.
	ak.accounts[100] = accounttypes.Account{AccountIndex: 100, Collateral: math.NewInt(300)}
	ak.pos[[2]uint64{100, 0}] = accounttypes.AccountPosition{
		AccountIndex: 100, MarketIndex: 0,
		BaseSize: math.NewInt(50), EntryQuote: math.NewInt(10_000),
		LastFundingRatePrefixSum: math.ZeroInt(), AllocatedMargin: math.ZeroInt(),
	}
	rk := newStubRisk()
	rk.status = perptypes.HealthBankruptcy
	rk.zero[[2]uint64{100, 0}] = 100
	rk.zero[[2]uint64{999, 0}] = 110
	// At default markPrice=100, cand 999 (-10, -2000) → uPnL=1000 (>0).
	rk.postSim[perptypes.InsuranceFundOperatorAccountIdx] = risktypes.RiskParameters{
		Collateral:                   math.NewInt(100),
		TotalAccountValue:            math.NewInt(50),
		InitialMarginRequirement:     math.NewInt(500), // breaches IMR
		MaintenanceMarginRequirement: math.NewInt(250),
		CloseOutMarginRequirement:    math.NewInt(125),
	}
	tk := &stubTrade{}
	matchk := newStubMatching()
	k, ctx := newKeeper(t, ak, rk, tk, matchk)

	require.NoError(t, k.EndBlocker(ctx))

	// No LLP takeover.
	for _, f := range tk.calls {
		require.NotEqual(t, perptypes.InsuranceFundOperatorAccountIdx, f.TakerAccountIndex,
			"LLP must not be taker when IMR check fails")
	}
	require.NotEmpty(t, tk.calls)
	require.Equal(t, uint64(999), tk.calls[0].TakerAccountIndex)
}

// TestEndBlocker_PreLiquidationClearsFlags ensures the EndBlocker
// drops stale liquidation flags as soon as the account's health
// recovers to PRE / HEALTHY (no flag should persist into a healthy
// account's records).
func TestEndBlocker_PreLiquidationClearsFlags(t *testing.T) {
	ak := newStubAccount()
	ak.accounts[100] = accounttypes.Account{AccountIndex: 100, Collateral: math.NewInt(10_000)}
	ak.pos[[2]uint64{100, 0}] = accounttypes.AccountPosition{
		AccountIndex: 100, MarketIndex: 0,
		BaseSize: math.NewInt(5), EntryQuote: math.NewInt(-50),
		LastFundingRatePrefixSum: math.ZeroInt(), AllocatedMargin: math.ZeroInt(),
	}
	rk := newStubRisk()
	rk.status = perptypes.HealthPreLiquidation
	tk := &stubTrade{}
	matchk := newStubMatching()
	k, ctx := newKeeper(t, ak, rk, tk, matchk)

	require.NoError(t, k.EndBlocker(ctx))
	// PRE → no fill, no flag.
	require.Empty(t, tk.calls)
}

// TestEndBlocker_BankruptResidueStaysWithVictim covers the worst-case
// path: a bankrupt account whose LLP takeover would breach the IF's
// IMR AND whose ADL queue is empty (no profitable opposite-side
// counterparties). The position simply remains open and is
// re-evaluated next block; there is no chain-level rescue that
// drains the IF to make the bankrupt's collateral non-negative.
//
// Pre-fix behaviour: the EndBlocker would silently move the residual
// negative collateral to the IF (which itself has no balance check)
// regardless of the LLP IMR gate's verdict, completely defeating the
// LLP→ADL waterfall. Post-fix: ledger values are untouched.
func TestEndBlocker_BankruptResidueStaysWithVictim(t *testing.T) {
	ak := newStubAccount()
	// IF that would breach IMR if it took over the position.
	ak.accounts[perptypes.InsuranceFundOperatorAccountIdx] = accounttypes.Account{
		AccountIndex: perptypes.InsuranceFundOperatorAccountIdx,
		AccountType:  perptypes.InsuranceFundAccountType,
		Collateral:   math.NewInt(100),
		PublicPoolInfo: &accounttypes.PublicPoolInfo{
			Status: perptypes.PublicPoolStatusActive,
		},
	}
	// Bankrupt victim with deeply negative collateral and no ADL
	// counterparty at all — autoADL must walk an empty queue.
	ak.accounts[100] = accounttypes.Account{
		AccountIndex: 100, Collateral: math.NewInt(-200),
	}
	ak.pos[[2]uint64{100, 0}] = accounttypes.AccountPosition{
		AccountIndex: 100, MarketIndex: 0,
		BaseSize: math.NewInt(50), EntryQuote: math.NewInt(-10_000),
		LastFundingRatePrefixSum: math.ZeroInt(), AllocatedMargin: math.ZeroInt(),
	}
	rk := newStubRisk()
	rk.status = perptypes.HealthBankruptcy
	rk.zero[[2]uint64{100, 0}] = 100
	// Single victim position; ranking is trivial. At default
	// markPrice=100 the (50, -10000) long realises uPnL = +15000 (offset
	// by entry sign convention), but only the sign matters for the
	// "LLP first" preflight here.
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

	// LLP refused (IMR breach) and ADL queue is empty: no fill
	// should have been issued at all.
	require.Empty(t, tk.calls,
		"no fill expected when LLP rejects and ADL queue is empty")
	// Both ledgers must be exactly as they started — there is no
	// silent top-up of bankruptcy losses out of the IF.
	require.True(t, ak.accounts[100].Collateral.Equal(math.NewInt(-200)),
		"victim residual debt must persist (got=%s)",
		ak.accounts[100].Collateral.String(),
	)
	require.True(t,
		ak.accounts[perptypes.InsuranceFundOperatorAccountIdx].Collateral.Equal(math.NewInt(100)),
		"IF collateral must not be debited as a post-block sweep (got=%s)",
		ak.accounts[perptypes.InsuranceFundOperatorAccountIdx].Collateral.String(),
	)
}

// TestEndBlocker_ADLCandidateInsufficientCollateral_AdvancesToNext
// covers Gap C 内 autoADL: when the first ADL candidate's collateral
// cannot cover the close-out at the candidate-specific settle price,
// autoADL must move on to the next candidate rather than aborting.
func TestEndBlocker_ADLCandidateInsufficientCollateral_AdvancesToNext(t *testing.T) {
	ak := newStubAccount()
	// IF that breaches IMR so EndBlocker delegates to autoADL.
	ak.accounts[perptypes.InsuranceFundOperatorAccountIdx] = accounttypes.Account{
		AccountIndex: perptypes.InsuranceFundOperatorAccountIdx,
		AccountType:  perptypes.InsuranceFundAccountType,
		Collateral:   math.NewInt(100),
		PublicPoolInfo: &accounttypes.PublicPoolInfo{
			Status: perptypes.PublicPoolStatusActive,
		},
	}
	// Bankrupt with sufficient cushion for ADL settle.
	ak.accounts[100] = accounttypes.Account{AccountIndex: 100, Collateral: math.NewInt(1_000)}
	ak.pos[[2]uint64{100, 0}] = accounttypes.AccountPosition{
		AccountIndex: 100, MarketIndex: 0,
		BaseSize: math.NewInt(50), EntryQuote: math.NewInt(5_000),
		LastFundingRatePrefixSum: math.ZeroInt(), AllocatedMargin: math.ZeroInt(),
	}
	// First candidate (highest profit rank) has zero cushion and
	// will trip the deleverager-side collateral assert. Picks a
	// slightly more negative EntryQuote (-2200) than 202 (-2000) so
	// that at markPrice=100 its uPnL ratio (=1200/2200) exceeds 202's
	// (=1000/2000) and it ranks first in BuildADLQueue.
	ak.accounts[201] = accounttypes.Account{
		AccountIndex: 201, AccountType: perptypes.MasterAccountType,
		Collateral: math.NewInt(0),
	}
	ak.pos[[2]uint64{201, 0}] = accounttypes.AccountPosition{
		AccountIndex: 201, MarketIndex: 0,
		BaseSize: math.NewInt(-10), EntryQuote: math.NewInt(-2_200),
		LastFundingRatePrefixSum: math.ZeroInt(), AllocatedMargin: math.ZeroInt(),
	}
	// Second candidate has a deep cushion and matches.
	ak.accounts[202] = accounttypes.Account{
		AccountIndex: 202, AccountType: perptypes.MasterAccountType,
		Collateral: math.NewInt(1_000_000),
	}
	ak.pos[[2]uint64{202, 0}] = accounttypes.AccountPosition{
		AccountIndex: 202, MarketIndex: 0,
		BaseSize: math.NewInt(-10), EntryQuote: math.NewInt(-2_000),
		LastFundingRatePrefixSum: math.ZeroInt(), AllocatedMargin: math.ZeroInt(),
	}
	rk := newStubRisk()
	// Only the bankrupt (100) is in FULL_LIQUIDATION — the ADL
	// counterparties (201, 202) are healthy.
	rk.status = perptypes.HealthHealthy
	rk.statuses[100] = perptypes.HealthFullLiquidation
	rk.zero[[2]uint64{100, 0}] = 100
	rk.zero[[2]uint64{201, 0}] = 110
	rk.zero[[2]uint64{202, 0}] = 110
	rk.postSim[perptypes.InsuranceFundOperatorAccountIdx] = risktypes.RiskParameters{
		Collateral:                   math.NewInt(100),
		TotalAccountValue:            math.NewInt(50),
		InitialMarginRequirement:     math.NewInt(500),
		MaintenanceMarginRequirement: math.NewInt(250),
		CloseOutMarginRequirement:    math.NewInt(125),
	}
	tk := &stubTrade{}
	matchk := newStubMatching()
	k, ctx := newKeeper(t, ak, rk, tk, matchk)

	require.NoError(t, k.EndBlocker(ctx))

	require.NotEmpty(t, tk.calls,
		"second ADL candidate must take over after the first one is rejected")
	for _, f := range tk.calls {
		require.NotEqual(t, uint64(201), f.TakerAccountIndex,
			"candidate 201 had insufficient collateral; must have been skipped")
	}
	require.Equal(t, uint64(202), tk.calls[0].TakerAccountIndex)
}

// TestEndBlocker_CrossAggregateRefreshedAcrossMarkets pins the
// cross-aggregate staleness invariant: processAccount must NOT carry
// pre-mutation cross RiskParameters or status across markets when
// the previous market's fill has just shifted them.
//
// Setup: account 100 holds two cross positions (markets 0 and 1) and
// is FULL_LIQUIDATION at the start of the block. The LLP can absorb
// both. After the FIRST absorption (market 0) the stubbed trade
// engine flips the account to HEALTHY — modelling the realised PnL
// having lifted TAV above IMR. The fix here is twofold: (1)
// processAccount calls `refreshHealth` per cross position right
// before the LLP/ADL waterfall fires, so the second market's
// trigger sees the post-mutation HEALTHY status and skips entirely;
// (2) autoADL self-asserts on its own snapshot's risk envelope as
// defense-in-depth (covered separately by
// TestAutoADL_RefusesHealedVictimViaSelfAssert).
func TestEndBlocker_CrossAggregateRefreshedAcrossMarkets(t *testing.T) {
	ak := newStubAccount()
	ak.accounts[perptypes.InsuranceFundOperatorAccountIdx] = accounttypes.Account{
		AccountIndex: perptypes.InsuranceFundOperatorAccountIdx,
		AccountType:  perptypes.InsuranceFundAccountType,
		Collateral:   math.NewInt(10_000_000),
		PublicPoolInfo: &accounttypes.PublicPoolInfo{
			Status:         perptypes.PublicPoolStatusActive,
			TotalShares:    math.NewInt(1),
			OperatorShares: math.NewInt(1),
		},
	}
	ak.accounts[100] = accounttypes.Account{AccountIndex: 100, Collateral: math.NewInt(10)}
	// Two FULL_LIQUIDATION cross positions. Market 0 is the worst
	// (uPnL = pos*markPrice - EQ = 50*100 - 10_000 = -5_000 at markPrice=100);
	// market 1 is less bad (uPnL = 5*100 - 1_000 = -500). The
	// LLP-takeover ranking and the persisted-position iterator both
	// process market 0 first, so the post-fill mutation we install
	// below fires before market 1 is reached.
	ak.pos[[2]uint64{100, 0}] = accounttypes.AccountPosition{
		AccountIndex: 100, MarketIndex: 0,
		BaseSize: math.NewInt(50), EntryQuote: math.NewInt(10_000),
		LastFundingRatePrefixSum: math.ZeroInt(), AllocatedMargin: math.ZeroInt(),
	}
	ak.pos[[2]uint64{100, 1}] = accounttypes.AccountPosition{
		AccountIndex: 100, MarketIndex: 1,
		BaseSize: math.NewInt(5), EntryQuote: math.NewInt(1_000),
		LastFundingRatePrefixSum: math.ZeroInt(), AllocatedMargin: math.ZeroInt(),
	}
	rk := newStubRisk()
	rk.statuses[100] = perptypes.HealthFullLiquidation

	// Simulate the cross account flipping HEALTHY after the first
	// absorption. A keeper that cached crossRP/status from the start
	// of processAccount would still issue a second fill against the
	// (no longer bankrupt) account.
	tk := &stubTrade{
		onCall: func(f tradekeeper.PerpFill) {
			if f.MarketIndex == 0 {
				rk.statuses[100] = perptypes.HealthHealthy
				rk.cross[100] = riskParamsForStatus(perptypes.HealthHealthy)
			}
		},
	}
	matchk := newStubMatching()
	k, ctx := newKeeper(t, ak, rk, tk, matchk)

	require.NoError(t, k.EndBlocker(ctx))

	require.Len(t, tk.calls, 1,
		"only market 0 should fill: market 1 must observe the post-mutation HEALTHY status and skip")
	require.Equal(t, uint32(0), tk.calls[0].MarketIndex)
	require.GreaterOrEqual(t, rk.snapshotCalls, 1,
		"market 0's LLP path must build at least one fresh risk snapshot before the fill")
}
