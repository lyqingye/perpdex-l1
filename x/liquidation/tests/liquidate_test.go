// MsgLiquidate / Keeper.Liquidate (partial-liquidation IOC) coverage:
//
//   - Input validation: oversized base_amount and zero base_amount must
//     reject before the matching engine is touched.
//   - Status gating: only PARTIAL_LIQUIDATION accounts are serviced by
//     this path; FULL/BANKRUPTCY victims must fall through to the
//     EndBlocker LLP→ADL waterfall.
//   - Operational flow: the victim's resting orders are cancelled
//     before the IOC is dispatched; the matching keeper receives the
//     correct close-out parameters with the LLP / Insurance Fund as
//     fee recipient; the legacy direct ApplyPerpsMatching path is NOT
//     reused.
//   - IF invariance: the partial-liquidation path NEVER reaches into
//     the Insurance Fund as a post-trade top-up, even when the victim
//     starts with a pre-existing negative collateral balance.
package tests

import (
	"testing"

	"cosmossdk.io/math"

	"github.com/stretchr/testify/require"

	perptypes "github.com/perpdex/perpdex-l1/types"
	accounttypes "github.com/perpdex/perpdex-l1/x/account/types"
	liqtypes "github.com/perpdex/perpdex-l1/x/liquidation/types"
)

// TestLiquidate_BaseAmountCappedByPosition ensures that passing a
// base_amount greater than the victim's position is rejected rather
// than silently flipping the position.
func TestLiquidate_BaseAmountCappedByPosition(t *testing.T) {
	ak := newStubAccount()
	ak.accounts[100] = accounttypes.Account{AccountIndex: 100, Collateral: math.ZeroInt()}
	ak.pos[[2]uint64{100, 0}] = accounttypes.AccountPosition{
		AccountIndex: 100, MarketIndex: 0,
		BaseSize: math.NewInt(5), EntryQuote: math.NewInt(-50),
		LastFundingRatePrefixSum: math.ZeroInt(), AllocatedMargin: math.ZeroInt(),
	}
	rk := newStubRisk()
	rk.status = perptypes.HealthPartialLiquidation
	tk := &stubTrade{}
	matchk := newStubMatching()
	k, ctx := newKeeper(t, ak, rk, tk, matchk)

	err := k.Liquidate(ctx, 100, 0, 999 /* > victim size */)
	require.Error(t, err)
	require.ErrorIs(t, err, liqtypes.ErrInvalidParams)
	require.Empty(t, matchk.liqCalls,
		"oversized base must reject before MatchLiquidationOrder is invoked")
}

// TestLiquidate_ZeroBaseRejected pins the invariant that base_amount
// == 0 is rejected explicitly.
func TestLiquidate_ZeroBaseRejected(t *testing.T) {
	ak := newStubAccount()
	ak.accounts[100] = accounttypes.Account{AccountIndex: 100, Collateral: math.ZeroInt()}
	ak.pos[[2]uint64{100, 0}] = accounttypes.AccountPosition{
		AccountIndex: 100, MarketIndex: 0, BaseSize: math.NewInt(3),
		EntryQuote: math.NewInt(-30), LastFundingRatePrefixSum: math.ZeroInt(),
		AllocatedMargin: math.ZeroInt(),
	}
	rk := newStubRisk()
	rk.status = perptypes.HealthPartialLiquidation
	tk := &stubTrade{}
	matchk := newStubMatching()
	k, ctx := newKeeper(t, ak, rk, tk, matchk)

	err := k.Liquidate(ctx, 100, 0, 0)
	require.ErrorIs(t, err, liqtypes.ErrInvalidParams)
}

// TestLiquidate_RejectsFullLiquidationStatus verifies that MsgLiquidate
// only services PARTIAL_LIQUIDATION; FULL/BANKRUPTCY accounts must
// fall through to the EndBlocker LLP→ADL waterfall
// (`InternalDeleverageTx` path).
func TestLiquidate_RejectsFullLiquidationStatus(t *testing.T) {
	ak := newStubAccount()
	ak.accounts[100] = accounttypes.Account{AccountIndex: 100, Collateral: math.ZeroInt()}
	ak.pos[[2]uint64{100, 0}] = accounttypes.AccountPosition{
		AccountIndex: 100, MarketIndex: 0, BaseSize: math.NewInt(3),
		EntryQuote: math.NewInt(-30), LastFundingRatePrefixSum: math.ZeroInt(),
		AllocatedMargin: math.ZeroInt(),
	}
	rk := newStubRisk()
	rk.status = perptypes.HealthFullLiquidation
	tk := &stubTrade{}
	matchk := newStubMatching()
	k, ctx := newKeeper(t, ak, rk, tk, matchk)

	err := k.Liquidate(ctx, 100, 0, 1)
	require.ErrorIs(t, err, liqtypes.ErrNotLiquidatable)
	require.Empty(t, matchk.liqCalls,
		"FULL victim must not enter the matching path")

	rk.status = perptypes.HealthBankruptcy
	err = k.Liquidate(ctx, 100, 0, 1)
	require.ErrorIs(t, err, liqtypes.ErrNotLiquidatable,
		"BANKRUPTCY must also reject the partial-liquidation route")
}

// TestLiquidate_CancelsVictimOpenOrders verifies the spec rule "first
// cancel all open orders of the user" before submitting the
// liquidation IOC to the matching keeper, matching
// `InternalCancelAllOrdersTx → InternalLiquidatePositionTx`.
func TestLiquidate_CancelsVictimOpenOrders(t *testing.T) {
	ak := newStubAccount()
	ak.accounts[100] = accounttypes.Account{AccountIndex: 100, Collateral: math.ZeroInt()}
	ak.pos[[2]uint64{100, 0}] = accounttypes.AccountPosition{
		AccountIndex: 100, MarketIndex: 0,
		BaseSize: math.NewInt(10), EntryQuote: math.NewInt(-100),
		LastFundingRatePrefixSum: math.ZeroInt(), AllocatedMargin: math.ZeroInt(),
	}
	rk := newStubRisk()
	rk.status = perptypes.HealthPartialLiquidation
	tk := &stubTrade{}
	matchk := newStubMatching()
	k, ctx := newKeeper(t, ak, rk, tk, matchk)

	err := k.Liquidate(ctx, 100, 0, 5)
	require.NoError(t, err)
	require.Equal(t, uint32(1), matchk.cancelled[100],
		"victim must have orders cancelled before close-out")
	require.Len(t, matchk.liqCalls, 1,
		"matching keeper must receive exactly one liquidation IOC")
}

// TestLiquidate_DelegatesToMatchingKeeperWithLLPRecipient verifies
// that Keeper.Liquidate forwards the close-out to the matching keeper
// with the right victim / market / zero price / fee bps and routes the
// improvement fee to the LLP / Insurance Fund operator account.
func TestLiquidate_DelegatesToMatchingKeeperWithLLPRecipient(t *testing.T) {
	ak := newStubAccount()
	ak.accounts[100] = accounttypes.Account{AccountIndex: 100, Collateral: math.ZeroInt()}
	ak.pos[[2]uint64{100, 0}] = accounttypes.AccountPosition{
		AccountIndex: 100, MarketIndex: 0,
		BaseSize: math.NewInt(10), EntryQuote: math.NewInt(-1000),
		LastFundingRatePrefixSum: math.ZeroInt(), AllocatedMargin: math.ZeroInt(),
	}
	rk := newStubRisk()
	rk.status = perptypes.HealthPartialLiquidation
	rk.zero[[2]uint64{100, 0}] = 95 // markPrice-based zero price
	tk := &stubTrade{}
	matchk := newStubMatching()
	k, ctx := newKeeper(t, ak, rk, tk, matchk)

	err := k.Liquidate(ctx, 100, 0, 4)
	require.NoError(t, err)
	require.Len(t, matchk.liqCalls, 1)
	got := matchk.liqCalls[0]
	require.Equal(t, uint64(100), got.Victim)
	require.Equal(t, uint32(0), got.MarketIdx)
	require.Equal(t, uint32(95), got.ZeroPrice)
	require.Equal(t, uint64(4), got.BaseAmount)
	require.Greater(t, got.LiquidationFeeBps, uint32(0),
		"market.LiquidationFee must populate the fee bps on the IOC call")
	require.Equal(t, perptypes.InsuranceFundOperatorAccountIdx, got.LiquidationFeeRecipient,
		"liquidation fee must route to LLP / Insurance Fund operator")
	// Liquidation routes through the orderbook IOC path (matching
	// keeper); it MUST NOT call ApplyPerpsMatching directly.
	require.Empty(t, tk.calls,
		"liquidation must not bypass the orderbook IOC route")
}

// TestLiquidate_DoesNotTopUpFromIF is the positive assertion that the
// partial-liquidation path NEVER pulls collateral from the Insurance
// Fund as a post-trade safety net.
// `internal_liquidate_position.rs` only inserts a `LIQUIDATION_ORDER +
// IOC + reduce_only` and lets the matching engine settle improvements
// above zero_price; the chain has no "absorbNegativeCollateral" sweep
// that could silently transfer the deficit to the IF without an IMR
// gate.
//
// To make the assertion concrete we deliberately seed the victim with
// a pre-existing negative collateral value and assert that both the
// victim's and the IF's balances are left untouched.
func TestLiquidate_DoesNotTopUpFromIF(t *testing.T) {
	ak := newStubAccount()
	ak.accounts[perptypes.InsuranceFundOperatorAccountIdx] = accounttypes.Account{
		AccountIndex: perptypes.InsuranceFundOperatorAccountIdx,
		AccountType:  perptypes.InsuranceFundAccountType,
		Collateral:   math.NewInt(1_000_000),
	}
	ak.accounts[100] = accounttypes.Account{
		AccountIndex: 100, Collateral: math.NewInt(-50),
	}
	ak.pos[[2]uint64{100, 0}] = accounttypes.AccountPosition{
		AccountIndex: 100, MarketIndex: 0,
		BaseSize: math.NewInt(10), EntryQuote: math.NewInt(-100),
		LastFundingRatePrefixSum: math.ZeroInt(), AllocatedMargin: math.ZeroInt(),
	}
	rk := newStubRisk()
	rk.status = perptypes.HealthPartialLiquidation
	tk := &stubTrade{}
	matchk := newStubMatching()
	k, ctx := newKeeper(t, ak, rk, tk, matchk)

	require.NoError(t, k.Liquidate(ctx, 100, 0, 5))

	// Matching IOC must have been driven, but the IF account's
	// collateral and the victim's pre-existing deficit must both be
	// untouched: there is no post-trade collateral movement in the
	// partial-liquidation path.
	require.Len(t, matchk.liqCalls, 1, "partial liq must drive matching IOC exactly once")
	require.True(t,
		ak.accounts[perptypes.InsuranceFundOperatorAccountIdx].Collateral.Equal(math.NewInt(1_000_000)),
		"IF collateral must not be debited as a post-trade top-up (got=%s)",
		ak.accounts[perptypes.InsuranceFundOperatorAccountIdx].Collateral.String(),
	)
	require.True(t, ak.accounts[100].Collateral.Equal(math.NewInt(-50)),
		"victim's pre-existing negative collateral must persist (got=%s)",
		ak.accounts[100].Collateral.String(),
	)
}
