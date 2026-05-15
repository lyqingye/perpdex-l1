// Auto-deleveraging (ADL) coverage:
//
//   - BuildADLQueue ranking: candidates ordered by leverage * uPnL
//     ratio (high-leverage profitable counterparties ranked first).
//   - autoADL ZP-alignment guard: counterparties whose zero prices do
//     not overlap with the victim's are skipped so the close-out
//     cannot worsen the counterparty.
//   - autoADL self-assertion: even when processAccount has cached a
//     FULL_LIQUIDATION trigger, autoADL must re-classify the victim
//     from its own fresh snapshot before firing.
//   - Direct Keeper.Deleverage flow: IF/LLP deleveragers, user-ADL
//     deleveragers, and the cross-cutting risk-regression / collateral
//     asserts that gate them.
package tests

import (
	"testing"

	"cosmossdk.io/math"

	"github.com/stretchr/testify/require"

	perptypes "github.com/perpdex/perpdex-l1/types"
	accounttypes "github.com/perpdex/perpdex-l1/x/account/types"
	liqtypes "github.com/perpdex/perpdex-l1/x/liquidation/types"
	risktypes "github.com/perpdex/perpdex-l1/x/risk/types"
)

// TestAutoADL_RequiresZeroPriceAlignment verifies that ADL skips
// counterparties whose zero prices do NOT overlap with the victim's,
// which prevents the close-out from worsening the counterparty.
//
// The bankrupt is given a small but non-trivial collateral cushion
// so that any bankrupt-side collateral check passes at the candidate
// settle price; the test's interest is purely the ZP alignment
// filter, not the collateral guard.
func TestAutoADL_RequiresZeroPriceAlignment(t *testing.T) {
	ak := newStubAccount()
	ak.accounts[100] = accounttypes.Account{AccountIndex: 100, Collateral: math.NewInt(200)}
	ak.pos[[2]uint64{100, 0}] = accounttypes.AccountPosition{
		AccountIndex: 100, MarketIndex: 0,
		BaseSize: math.NewInt(50), EntryQuote: math.NewInt(5_000),
		LastFundingRatePrefixSum: math.ZeroInt(), AllocatedMargin: math.ZeroInt(),
	}
	// Two opposite-side candidates, but only one has an aligned ZP.
	ak.accounts[201] = accounttypes.Account{
		AccountIndex: 201, AccountType: perptypes.MasterAccountType,
		Collateral: math.NewInt(1_000_000),
	}
	ak.pos[[2]uint64{201, 0}] = accounttypes.AccountPosition{
		AccountIndex: 201, MarketIndex: 0,
		BaseSize: math.NewInt(-10), EntryQuote: math.NewInt(-1_500),
		LastFundingRatePrefixSum: math.ZeroInt(), AllocatedMargin: math.ZeroInt(),
	}
	ak.accounts[202] = accounttypes.Account{
		AccountIndex: 202, AccountType: perptypes.MasterAccountType,
		Collateral: math.NewInt(1_000_000),
	}
	ak.pos[[2]uint64{202, 0}] = accounttypes.AccountPosition{
		AccountIndex: 202, MarketIndex: 0,
		BaseSize: math.NewInt(-20), EntryQuote: math.NewInt(-2_500),
		LastFundingRatePrefixSum: math.ZeroInt(), AllocatedMargin: math.ZeroInt(),
	}
	rk := newStubRisk()
	rk.status = perptypes.HealthBankruptcy
	rk.zero[[2]uint64{100, 0}] = 100 // victim long ZP = 100 (need ZP_cand >= 100)
	rk.zero[[2]uint64{201, 0}] = 90  // misaligned: ZP < victim — skip
	rk.zero[[2]uint64{202, 0}] = 105 // aligned
	// At default markPrice=100, both shorts (Position=-10, EQ=-1500) and
	// (Position=-20, EQ=-2500) have positive uPnL (500 each), so they
	// both qualify as ADL candidates.
	tk := &stubTrade{}
	matchk := newStubMatching()
	k, ctx := newKeeper(t, ak, rk, tk, matchk)

	require.NoError(t, k.EndBlocker(ctx))

	// ADL should have happened against 202 only, at midpoint of 100 and
	// 105 = 102.
	require.NotEmpty(t, tk.calls)
	for _, f := range tk.calls {
		require.NotEqual(t, uint64(201), f.TakerAccountIndex,
			"misaligned ZP candidate must be skipped")
	}
	require.Equal(t, uint64(202), tk.calls[0].TakerAccountIndex)
	require.Equal(t, uint32(102), tk.calls[0].Price)
}

// TestADLQueueBuilder_LeverageAndUPnLRanking verifies the new ranking
// semantics: candidates are ordered by leverage * uPnL_ratio desc.
func TestADLQueueBuilder_LeverageAndUPnLRanking(t *testing.T) {
	ak := newStubAccount()
	// Two opposite-side longs with identical uPnL but different
	// leverages — higher leverage must come first.
	ak.accounts[201] = accounttypes.Account{
		AccountIndex: 201, AccountType: perptypes.MasterAccountType,
		Collateral: math.NewInt(10_000_000), // low leverage
	}
	ak.accounts[202] = accounttypes.Account{
		AccountIndex: 202, AccountType: perptypes.MasterAccountType,
		Collateral: math.NewInt(100_000), // high leverage
	}
	ak.pos[[2]uint64{201, 0}] = accounttypes.AccountPosition{
		AccountIndex: 201, MarketIndex: 0,
		BaseSize: math.NewInt(10), EntryQuote: math.NewInt(1_000),
		LastFundingRatePrefixSum: math.ZeroInt(), AllocatedMargin: math.ZeroInt(),
	}
	ak.pos[[2]uint64{202, 0}] = accounttypes.AccountPosition{
		AccountIndex: 202, MarketIndex: 0,
		BaseSize: math.NewInt(10), EntryQuote: math.NewInt(1_000),
		LastFundingRatePrefixSum: math.ZeroInt(), AllocatedMargin: math.ZeroInt(),
	}
	rk := newStubRisk()
	// Set markPrice=110 so both candidates' positions (Pos=10, EQ=1000)
	// realise uPnL=100 (=10*110-1000), giving an equal uPnLRatio so
	// ranking is decided purely by leverage (higher first).
	rk.marks[0] = 110
	rk.cross[201] = risktypes.RiskParameters{
		Collateral:                   math.NewInt(10_000_000),
		TotalAccountValue:            math.NewInt(10_000_000),
		InitialMarginRequirement:     math.NewInt(1_000),
		MaintenanceMarginRequirement: math.NewInt(500),
		CloseOutMarginRequirement:    math.NewInt(250),
	}
	rk.cross[202] = risktypes.RiskParameters{
		Collateral:                   math.NewInt(100_000),
		TotalAccountValue:            math.NewInt(100_000),
		InitialMarginRequirement:     math.NewInt(1_000),
		MaintenanceMarginRequirement: math.NewInt(500),
		CloseOutMarginRequirement:    math.NewInt(250),
	}
	tk := &stubTrade{}
	matchk := newStubMatching()
	k, ctx := newKeeper(t, ak, rk, tk, matchk)

	cands, err := k.BuildADLQueue(ctx, 0, true /* oppositeIsLong: victim is short */, 4)
	require.NoError(t, err)
	require.Len(t, cands, 2)
	require.Equal(t, uint64(202), cands[0].AccountIndex,
		"higher-leverage candidate must rank first")
	require.Equal(t, uint64(201), cands[1].AccountIndex)
}

// TestAutoADL_RefusesHealedVictimViaSelfAssert pins the autoADL
// self-gate invariant: even if processAccount has already decided
// the victim is FULL_LIQUIDATION and tryLLPAbsorb has rejected the
// absorption, autoADL MUST re-classify the victim's envelope from
// its OWN fresh snapshot. The trade engine's IsValidRiskChangeFrom
// accepts HEALTHY post-state unconditionally, so without this
// self-assertion a recovered victim could still be ADL'd against
// the engine's permissive path.
//
// We model an LLP-absorption FAILURE (postSim breach IMR) so the
// EndBlocker drops into the autoADL branch even though the victim
// is healing in real-time. The snapshot hook flips
// `statuses[100]` to HEALTHY on the SECOND snapshot call — the
// first is `tryLLPAbsorb`, the second is `autoADL` — so autoADL's
// own snap projects HEALTHY despite processAccount's cached trigger.
func TestAutoADL_RefusesHealedVictimViaSelfAssert(t *testing.T) {
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
	ak.pos[[2]uint64{100, 0}] = accounttypes.AccountPosition{
		AccountIndex: 100, MarketIndex: 0,
		BaseSize: math.NewInt(50), EntryQuote: math.NewInt(10_000),
		LastFundingRatePrefixSum: math.ZeroInt(), AllocatedMargin: math.ZeroInt(),
	}
	// Profitable counterparty so BuildADLQueue is non-empty: without
	// the self-assert, autoADL would happily fill against this
	// account. Short opened at 400 (negative EntryQuote per ApplyFill
	// sign convention) → uPnL = -50*100 - (-20_000) = +15_000.
	ak.accounts[999] = accounttypes.Account{AccountIndex: 999, Collateral: math.NewInt(1_000_000)}
	ak.pos[[2]uint64{999, 0}] = accounttypes.AccountPosition{
		AccountIndex: 999, MarketIndex: 0,
		BaseSize: math.NewInt(-50), EntryQuote: math.NewInt(-20_000),
		LastFundingRatePrefixSum: math.ZeroInt(), AllocatedMargin: math.ZeroInt(),
	}

	rk := newStubRisk()
	rk.statuses[100] = perptypes.HealthFullLiquidation
	rk.zero[[2]uint64{100, 0}] = 90
	rk.zero[[2]uint64{999, 0}] = 110
	// Force IF takeover to breach IMR so tryLLPAbsorb returns false
	// and the EndBlocker falls through to autoADL.
	rk.postSim[perptypes.InsuranceFundOperatorAccountIdx] = risktypes.RiskParameters{
		Collateral:                   math.NewInt(1),
		CollateralWithFunding:        math.NewInt(1),
		TotalAccountValue:            math.NewInt(1),
		InitialMarginRequirement:     math.NewInt(1_000_000_000),
		MaintenanceMarginRequirement: math.ZeroInt(),
		CloseOutMarginRequirement:    math.ZeroInt(),
	}
	// Flip the victim to HEALTHY on the second snapshot call. Call
	// #1 is tryLLPAbsorb; call #2 is autoADL. The flip happens
	// AFTER refreshHealth has already locked in the trigger
	// decision, so this isolates the self-assert path.
	rk.onSnapshot = func(s *stubRisk, acc uint64, _ uint32) {
		if acc == 100 && s.snapshotCalls == 2 {
			s.statuses[100] = perptypes.HealthHealthy
			s.cross[100] = riskParamsForStatus(perptypes.HealthHealthy)
		}
	}
	tk := &stubTrade{}
	matchk := newStubMatching()
	k, ctx := newKeeper(t, ak, rk, tk, matchk)

	require.NoError(t, k.EndBlocker(ctx))
	require.Empty(t, tk.calls,
		"autoADL must self-refuse a victim whose own snapshot says HEALTHY, regardless of the trigger that brought us here")
}

// TestDeleverage_LeavesResidualOnVictim covers the FULL/BANKRUPTCY
// arm: even though the LLP / IF participates as the deleverage
// counterparty, any residual negative collateral that may exist on
// the victim's ledger after the trade settles must NOT be silently
// transferred to the IF. The deleverage path settles at the
// victim's zero price and lets the bankrupt's ledger reflect the
// truth; there is no post-block IF top-up sweep.
func TestDeleverage_LeavesResidualOnVictim(t *testing.T) {
	ak := newStubAccount()
	ak.accounts[perptypes.InsuranceFundOperatorAccountIdx] = accounttypes.Account{
		AccountIndex: perptypes.InsuranceFundOperatorAccountIdx,
		AccountType:  perptypes.InsuranceFundAccountType,
		Collateral:   math.NewInt(1_000_000),
		PublicPoolInfo: &accounttypes.PublicPoolInfo{
			Status:         perptypes.PublicPoolStatusActive,
			TotalShares:    math.NewInt(1),
			OperatorShares: math.NewInt(1),
		},
	}
	// Bankrupt with a residual debt (-75) plus enough remaining
	// collateral to absorb the close-out's predicted realised PnL
	// at zeroPrice=10 (the stubRisk default). EntryQuote uses the
	// production canonical sign so `ApplyFill` returns a consistent
	// value with the engine.
	ak.accounts[100] = accounttypes.Account{
		AccountIndex: 100, Collateral: math.NewInt(-75),
	}
	ak.pos[[2]uint64{100, 0}] = accounttypes.AccountPosition{
		AccountIndex: 100, MarketIndex: 0,
		BaseSize: math.NewInt(20), EntryQuote: math.NewInt(2_000),
		LastFundingRatePrefixSum: math.ZeroInt(), AllocatedMargin: math.ZeroInt(),
	}
	rk := newStubRisk()
	rk.status = perptypes.HealthFullLiquidation
	tk := &stubTrade{}
	matchk := newStubMatching()
	k, ctx := newKeeper(t, ak, rk, tk, matchk)

	require.NoError(t, k.Deleverage(ctx, 100, 0, perptypes.InsuranceFundOperatorAccountIdx, 20))

	// Exactly one fill (LLP as taker, victim as maker), and neither
	// side's ledger value moves outside of what ApplyPerpsMatching
	// itself would have done — the stub trade engine does not touch
	// collateral, so any post-trade collateral mutation here would
	// have come from a `absorbNegativeCollateral` sweep that no
	// longer exists.
	require.Len(t, tk.calls, 1)
	require.Equal(t, perptypes.InsuranceFundOperatorAccountIdx, tk.calls[0].TakerAccountIndex)
	require.True(t, ak.accounts[100].Collateral.Equal(math.NewInt(-75)),
		"victim residual collateral must persist (got=%s)",
		ak.accounts[100].Collateral.String(),
	)
	require.True(t,
		ak.accounts[perptypes.InsuranceFundOperatorAccountIdx].Collateral.Equal(math.NewInt(1_000_000)),
		"IF collateral must not be debited beyond the trade itself (got=%s)",
		ak.accounts[perptypes.InsuranceFundOperatorAccountIdx].Collateral.String(),
	)
}

// TestDeleverage_BankruptRiskRegressionRejected pins the invariant
// that the bankrupt's post-trade IsValidRiskChangeFrom is enforced on
// the LLP path: if the close-out worsens TAV/MMR despite supposedly
// improving the account, the entire deleverage trade is aborted. The
// per-side SkipMakerRiskCheck flag never disables this check on a
// bankrupt account.
func TestDeleverage_BankruptRiskRegressionRejected(t *testing.T) {
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
	ak.accounts[100] = accounttypes.Account{AccountIndex: 100, Collateral: math.NewInt(10_000)}
	ak.pos[[2]uint64{100, 0}] = accounttypes.AccountPosition{
		AccountIndex: 100, MarketIndex: 0,
		BaseSize: math.NewInt(50), EntryQuote: math.NewInt(5_000),
		LastFundingRatePrefixSum: math.ZeroInt(), AllocatedMargin: math.ZeroInt(),
	}
	rk := newStubRisk()
	rk.status = perptypes.HealthFullLiquidation
	tk := &stubTrade{
		// Stub the engine to reject the deleverage with a maker-side
		// risk regression so the bankrupt-side post-trade risk check
		// is exercised.
		err: liqtypes.ErrInsuranceUnderfunded.Wrap("simulated bankrupt risk regression"),
	}
	matchk := newStubMatching()
	k, ctx := newKeeper(t, ak, rk, tk, matchk)

	err := k.Deleverage(ctx, 100, 0, perptypes.InsuranceFundOperatorAccountIdx, 50)
	require.Error(t, err,
		"bankrupt-side post-trade risk regression must abort the deleverage tx")

	// The flag-controlled checks on the engine call should have
	// requested bankrupt validation (SkipMakerRiskCheck=false) and
	// skipped the LLP/IF taker side (SkipTakerRiskCheck=true).
	require.Len(t, tk.calls, 1)
	require.False(t, tk.calls[0].SkipMakerRiskCheck,
		"bankrupt (maker) post-trade risk check must remain enabled in deleverage path")
	require.True(t, tk.calls[0].SkipTakerRiskCheck,
		"LLP / IF deleverager (taker) skips post-trade risk check")
}

// TestDeleverage_InsufficientDeleveragerCollateral_UserADL covers Gap C
// deleverager branch: under user-ADL the deleverager's own collateral
// is also asserted (perpdex defense-in-depth: the deleverager must
// have enough cross collateral to absorb the predicted realized
// loss). Insufficient collateral on the user-ADL deleverager
// rejects the trade.
//
// IF / pool deleveragers are NOT subject to this assert; that case is
// covered by the absence of an `ErrInsufficientCollateral` failure in
// `TestEndBlocker_FullLiquidationPrefersLLPThenADL`.
func TestDeleverage_InsufficientDeleveragerCollateral_UserADL(t *testing.T) {
	ak := newStubAccount()
	// Bankrupt with positive collateral so the bankrupt-side assert
	// passes by short-circuit.
	ak.accounts[100] = accounttypes.Account{AccountIndex: 100, Collateral: math.NewInt(1_000_000)}
	ak.pos[[2]uint64{100, 0}] = accounttypes.AccountPosition{
		AccountIndex: 100, MarketIndex: 0,
		BaseSize: math.NewInt(50), EntryQuote: math.NewInt(5_000),
		LastFundingRatePrefixSum: math.ZeroInt(), AllocatedMargin: math.ZeroInt(),
	}
	// User-ADL deleverager: opposite-side, but no cushion at all.
	// Closing their short at zeroPrice=10 against EQ=-5_000 yields
	// realised PnL ≈ -4_500 (in the engine's "Collateral += PnL"
	// frame) which they cannot cover.
	ak.accounts[200] = accounttypes.Account{
		AccountIndex: 200, AccountType: perptypes.MasterAccountType,
		Collateral: math.NewInt(0),
	}
	ak.pos[[2]uint64{200, 0}] = accounttypes.AccountPosition{
		AccountIndex: 200, MarketIndex: 0,
		BaseSize: math.NewInt(-50), EntryQuote: math.NewInt(-5_000),
		LastFundingRatePrefixSum: math.ZeroInt(), AllocatedMargin: math.ZeroInt(),
	}
	rk := newStubRisk()
	rk.status = perptypes.HealthFullLiquidation
	// Force a low zeroPrice (10) for the bankrupt so closing the
	// deleverager's short at that price realises ≈ -4500 in the
	// engine's "Collateral += PnL" frame (deleverager has 0 cushion).
	// Without this override the stub falls through to markPrice=100, at
	// which point the close PnL is zero and the assert short-circuits.
	rk.zero[[2]uint64{100, 0}] = 10
	tk := &stubTrade{}
	matchk := newStubMatching()
	k, ctx := newKeeper(t, ak, rk, tk, matchk)

	err := k.Deleverage(ctx, 100, 0, 200, 50)
	require.Error(t, err)
	require.ErrorIs(t, err, liqtypes.ErrInsufficientCollateral)
	require.Empty(t, tk.calls)
}
