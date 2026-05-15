// perp_apply_test.go covers the happy-path business behaviour of
// `tradekeeper.Keeper.ApplyPerpsMatching`: open-interest accounting on
// round-trip fills, the liquidation-fee ladder routed to the LLP /
// Insurance Fund, and the optional risk-check bypasses used by the
// keeper-driven deleverage / IF absorption flows.
//
// Sentinel error classification for the perps path lives in
// sentinel_test.go; isolated-margin allocation behaviour lives in
// isolated_margin_test.go.
package tests

import (
	"testing"

	"github.com/stretchr/testify/require"

	"cosmossdk.io/math"

	perptypes "github.com/perpdex/perpdex-l1/types"
	accounttypes "github.com/perpdex/perpdex-l1/x/account/types"
	tradekeeper "github.com/perpdex/perpdex-l1/x/trade/keeper"
	tradetypes "github.com/perpdex/perpdex-l1/x/trade/types"
)

// TestApplyPerpsMatching_OIRoundTrip verifies that opening and then closing
// a position nets open interest back to 0 (audit High trade-4).
func TestApplyPerpsMatching_OIRoundTrip(t *testing.T) {
	ctx, ak, mk, rk, k := newSdkCtx(t)
	_ = rk

	// Seed some collateral so fee deduction never tips either account into
	// the negative (risk keeper will accept because stub returns true).
	require.NoError(t, ak.SetAccount(ctx, accounttypes.Account{
		AccountIndex: 10, Collateral: math.NewInt(1_000_000),
	}))
	require.NoError(t, ak.SetAccount(ctx, accounttypes.Account{
		AccountIndex: 20, Collateral: math.NewInt(1_000_000),
	}))

	require.NoError(t, k.ApplyPerpsMatching(ctx, tradekeeper.PerpFill{
		MakerAccountIndex: 10, TakerAccountIndex: 20,
		MarketIndex: 1, Price: 100, BaseAmount: 7,
		IsTakerAsk: false, NoFee: true,
	}))
	require.Equal(t, int64(7), mk.oi[1])

	// Taker now closes against the same maker; OI must return to zero.
	require.NoError(t, k.ApplyPerpsMatching(ctx, tradekeeper.PerpFill{
		MakerAccountIndex: 10, TakerAccountIndex: 20,
		MarketIndex: 1, Price: 100, BaseAmount: 7,
		IsTakerAsk: true, NoFee: true,
	}))
	require.Equal(t, int64(0), mk.oi[1])
}

// TestApplyPerpsMatching_LiquidationFeeRoutesToLLP exercises the
// improvement-over-zero-price liquidation fee path. When the fill
// price is strictly better than the zero price for the victim, an
// improvement fee is debited from the side being closed and credited
// to LiquidationFeeRecipient (LLP / Insurance Fund). Standard
// taker/maker fees do not apply on the same fill (caller sets them
// to 0), so the treasury stays untouched.
//
// Numerical expectations:
//
//	improvement     = (Price - ZeroPrice) * BaseAmount = (110-100)*1000 = 10_000
//	notional        = Price * BaseAmount               = 110*1000       = 110_000
//	price_diff_rate = (|Price-ZeroPrice| * FeeTick) / Price
//	                = (10 * 1_000_000) / 110 ≈ 90_909
//	effective_rate  = min(LiquidationFeeBps=10_000, 90_909) = 10_000
//	fee             = notional * 10_000 / FeeTick
//	                = 110_000 * 10_000 / 1_000_000        = 1_100
//
// The cap drops out of the `min(LiquidationFeeBps, price_diff_rate)`
// rate, scaled across notional rather than across improvement — the
// per-trade taker fee bound enforced by
// `liquidationImprovementFee` in the trade engine.
func TestApplyPerpsMatching_LiquidationFeeRoutesToLLP(t *testing.T) {
	ctx, ak, _, _, k := newSdkCtx(t)
	const (
		victimIdx = uint64(100)
		takerIdx  = uint64(200)
		llpIdx    = perptypes.InsuranceFundOperatorAccountIdx
	)
	require.NoError(t, ak.SetAccount(ctx, accounttypes.Account{
		AccountIndex: victimIdx, Collateral: math.NewInt(10_000_000),
	}))
	require.NoError(t, ak.SetAccount(ctx, accounttypes.Account{
		AccountIndex: takerIdx, Collateral: math.NewInt(10_000_000),
	}))
	require.NoError(t, ak.SetAccount(ctx, accounttypes.Account{
		AccountIndex: llpIdx, Collateral: math.NewInt(0),
	}))
	require.NoError(t, k.ApplyPerpsMatching(ctx, tradekeeper.PerpFill{
		MakerAccountIndex:       victimIdx,
		TakerAccountIndex:       takerIdx,
		MarketIndex:             1,
		Price:                   110,
		BaseAmount:              1000,
		IsTakerAsk:              true, // taker sells, victim closes long
		ZeroPrice:               100,
		LiquidationFeeBps:       10_000, // 1% in fee tick units
		LiquidationFeeRecipient: llpIdx,
		NoFee:                   false,
		SkipMakerRiskCheck:      true,
	}))
	llp, err := ak.GetAccount(ctx, llpIdx)
	require.NoError(t, err)
	require.Equal(t, "1100", llp.Collateral.String(),
		"LLP must receive notional * min(bps, price_diff_rate) / FeeTick")
	treasury, err := ak.GetAccount(ctx, perptypes.TreasuryAccountIndex)
	require.NoError(t, err)
	require.True(t, treasury.Collateral.IsZero(),
		"Treasury must remain untouched on a fee-less liquidation fill")
}

// TestApplyPerpsMatching_LiquidationFeePriceDiffRateBound asserts the
// `min(LiquidationFeeBps, price_diff_rate)` ceiling: when the price
// improvement over the zero-price floor is small relative to the
// price (price_diff_rate < LiquidationFeeBps), the fee is bounded by
// price_diff_rate, NOT by the configured LiquidationFee. Without the
// rate bound, a tiny improvement at a high LiquidationFee would
// allow fees that exceed the actual gain.
//
// Setup: Price=101, ZeroPrice=100, BaseAmount=1000, fee_bps=50_000 (5%).
//
//	improvement     = 1 * 1000 = 1000
//	notional        = 101 * 1000 = 101_000
//	price_diff_rate = (1 * 1_000_000) / 101 ≈ 9_900
//	effective_rate  = min(50_000, 9_900) = 9_900
//	fee             = 101_000 * 9_900 / 1_000_000 = 999  (truncated)
//
// Without the price_diff_rate cap the fee would have been
// `notional * 50_000 / FeeTick = 5_050`, ~5x larger than the actual
// improvement — the bug the spec explicitly guards against.
func TestApplyPerpsMatching_LiquidationFeePriceDiffRateBound(t *testing.T) {
	ctx, ak, _, _, k := newSdkCtx(t)
	const (
		victimIdx = uint64(100)
		takerIdx  = uint64(200)
		llpIdx    = perptypes.InsuranceFundOperatorAccountIdx
	)
	require.NoError(t, ak.SetAccount(ctx, accounttypes.Account{
		AccountIndex: victimIdx, Collateral: math.NewInt(10_000_000),
	}))
	require.NoError(t, ak.SetAccount(ctx, accounttypes.Account{
		AccountIndex: takerIdx, Collateral: math.NewInt(10_000_000),
	}))
	require.NoError(t, ak.SetAccount(ctx, accounttypes.Account{
		AccountIndex: llpIdx, Collateral: math.NewInt(0),
	}))
	require.NoError(t, k.ApplyPerpsMatching(ctx, tradekeeper.PerpFill{
		MakerAccountIndex:       victimIdx,
		TakerAccountIndex:       takerIdx,
		MarketIndex:             1,
		Price:                   101,
		BaseAmount:              1000,
		IsTakerAsk:              true,
		ZeroPrice:               100,
		LiquidationFeeBps:       50_000, // 5%, deliberately oversized
		LiquidationFeeRecipient: llpIdx,
		SkipMakerRiskCheck:      true,
	}))
	llp, err := ak.GetAccount(ctx, llpIdx)
	require.NoError(t, err)
	require.Equal(t, "999", llp.Collateral.String(),
		"price_diff_rate must clip the LLP fee below the bps ceiling")
}

// TestApplyPerpsMatching_LiquidationFeeNoneAtZeroPrice asserts the
// edge case the partial-liquidation engine relies on: when the fill
// price equals the zero price (the keeper-driven IoC close-out path),
// the improvement is zero and no fee is charged.
func TestApplyPerpsMatching_LiquidationFeeNoneAtZeroPrice(t *testing.T) {
	ctx, ak, _, _, k := newSdkCtx(t)
	const llpIdx = perptypes.InsuranceFundOperatorAccountIdx
	require.NoError(t, ak.SetAccount(ctx, accounttypes.Account{
		AccountIndex: 100, Collateral: math.NewInt(1_000_000),
	}))
	require.NoError(t, ak.SetAccount(ctx, accounttypes.Account{
		AccountIndex: 200, Collateral: math.NewInt(1_000_000),
	}))
	require.NoError(t, ak.SetAccount(ctx, accounttypes.Account{
		AccountIndex: llpIdx, Collateral: math.NewInt(0),
	}))
	require.NoError(t, k.ApplyPerpsMatching(ctx, tradekeeper.PerpFill{
		MakerAccountIndex:       100,
		TakerAccountIndex:       200,
		MarketIndex:             1,
		Price:                   100,
		BaseAmount:              1000,
		IsTakerAsk:              true,
		ZeroPrice:               100, // fill == zero price
		LiquidationFeeBps:       10_000,
		LiquidationFeeRecipient: llpIdx,
		NoFee:                   false,
		SkipMakerRiskCheck:      true,
	}))
	llp, err := ak.GetAccount(ctx, llpIdx)
	require.NoError(t, err)
	require.True(t, llp.Collateral.IsZero(),
		"no improvement ⇒ no fee; LLP must not receive collateral")
}

// TestApplyPerpsMatching_SkipTakerRiskCheck verifies the flag
// introduced for `Deleverage`'s LLP / IF path: when the taker is the
// IF/LLP absorber, both the pre-trade snapshot and the post-trade
// `IsValidRiskChangeFrom` on the taker must be skipped (the absorber's
// collateral sufficiency is gated by `tryLLPAbsorb`'s pre-trade
// `SimulateRiskAfterTakeover`/IMR check and the
// `is_*_has_enough_cross_collateral` assert instead). The maker side
// keeps its full risk pipeline.
func TestApplyPerpsMatching_SkipTakerRiskCheck(t *testing.T) {
	ctx, ak, _, rk, k := newSdkCtx(t)
	require.NoError(t, ak.SetAccount(ctx, accounttypes.Account{
		AccountIndex: 10, Collateral: math.NewInt(1_000_000),
	}))
	require.NoError(t, ak.SetAccount(ctx, accounttypes.Account{
		AccountIndex: 20, Collateral: math.NewInt(1_000_000),
	}))

	require.NoError(t, k.ApplyPerpsMatching(ctx, tradekeeper.PerpFill{
		MakerAccountIndex:  10,
		TakerAccountIndex:  20,
		MarketIndex:        1,
		Price:              100,
		BaseAmount:         5,
		IsTakerAsk:         true,
		NoFee:              true,
		SkipTakerRiskCheck: true,
	}))
	// Only the maker should be snapshotted and risk-checked once;
	// the taker side is fully bypassed.
	require.Equal(t, 1, rk.snapshots,
		"taker pre-snapshot must be skipped under SkipTakerRiskCheck")
	require.Equal(t, 1, rk.riskChecks,
		"only the maker should run IsValidRiskChangeFrom under SkipTakerRiskCheck")

	// SkipMakerRiskCheck stays independent: turning ON SkipMaker as
	// well drops the post-check count to 0 (we still snapshot
	// nothing on either side, and never enter the post-trade loop).
	ctx2, ak2, _, rk2, k2 := newSdkCtx(t)
	require.NoError(t, ak2.SetAccount(ctx2, accounttypes.Account{
		AccountIndex: 10, Collateral: math.NewInt(1_000_000),
	}))
	require.NoError(t, ak2.SetAccount(ctx2, accounttypes.Account{
		AccountIndex: 20, Collateral: math.NewInt(1_000_000),
	}))
	require.NoError(t, k2.ApplyPerpsMatching(ctx2, tradekeeper.PerpFill{
		MakerAccountIndex:  10,
		TakerAccountIndex:  20,
		MarketIndex:        1,
		Price:              100,
		BaseAmount:         5,
		IsTakerAsk:         true,
		NoFee:              true,
		SkipMakerRiskCheck: true,
		SkipTakerRiskCheck: true,
	}))
	require.Equal(t, 0, rk2.snapshots,
		"both flags on => no snapshots at all")
	require.Equal(t, 0, rk2.riskChecks,
		"both flags on => no IsValidRiskChangeFrom")

	// Sanity: with SkipTakerRiskCheck OFF (default), a forced taker
	// rejection (rejectOnCall=1, the for-loop iterates [Taker, Maker])
	// surfaces as `ErrTakerRiskRegression`, confirming the taker leg
	// IS reachable when the flag isn't set.
	ctx3, ak3, _, rk3, k3 := newSdkCtx(t)
	require.NoError(t, ak3.SetAccount(ctx3, accounttypes.Account{
		AccountIndex: 10, Collateral: math.NewInt(1_000_000),
	}))
	require.NoError(t, ak3.SetAccount(ctx3, accounttypes.Account{
		AccountIndex: 20, Collateral: math.NewInt(1_000_000),
	}))
	rk3.rejectOnCall = 1
	err := k3.ApplyPerpsMatching(ctx3, tradekeeper.PerpFill{
		MakerAccountIndex: 10,
		TakerAccountIndex: 20,
		MarketIndex:       1,
		Price:             100,
		BaseAmount:        5,
		IsTakerAsk:        true,
		NoFee:             true,
	})
	require.Error(t, err)
	require.ErrorIs(t, err, tradetypes.ErrTakerRiskRegression,
		"baseline (no Skip*RiskCheck) MUST classify the rejection as taker regression")
}
