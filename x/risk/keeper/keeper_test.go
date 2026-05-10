package keeper_test

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"

	cmtprototypes "github.com/cometbft/cometbft/proto/tendermint/types"

	"cosmossdk.io/log"
	"cosmossdk.io/math"
	storetypes "cosmossdk.io/store/types"

	"github.com/cosmos/cosmos-sdk/runtime"
	"github.com/cosmos/cosmos-sdk/testutil/integration"
	sdk "github.com/cosmos/cosmos-sdk/types"
	moduletestutil "github.com/cosmos/cosmos-sdk/types/module/testutil"

	perptypes "github.com/perpdex/perpdex-l1/types"
	accounttypes "github.com/perpdex/perpdex-l1/x/account/types"
	markettypes "github.com/perpdex/perpdex-l1/x/market/types"
	oracletypes "github.com/perpdex/perpdex-l1/x/oracle/types"
	riskkeeper "github.com/perpdex/perpdex-l1/x/risk/keeper"
	risktypes "github.com/perpdex/perpdex-l1/x/risk/types"
)

type stubAccountKeeper struct {
	acc accounttypes.Account
	pos accounttypes.AccountPosition
}

// Pointer receivers so the test can mutate `acc`/`pos` AFTER
// constructing the keeper (e.g. for pre/post-state
// IsValidRiskChangeFrom scenarios) without rebuilding the whole
// keeper instance.
func (s *stubAccountKeeper) GetAccount(_ context.Context, _ uint64) (accounttypes.Account, error) {
	return s.acc, nil
}
func (s *stubAccountKeeper) GetPosition(_ context.Context, _ uint64, mkt uint32) (accounttypes.AccountPosition, error) {
	if mkt == s.pos.MarketIndex {
		return s.pos, nil
	}
	return accounttypes.AccountPosition{
		BaseSize: math.ZeroInt(), EntryQuote: math.ZeroInt(),
		LastFundingRatePrefixSum: math.ZeroInt(), AllocatedMargin: math.ZeroInt(),
	}, nil
}
func (s *stubAccountKeeper) GetAccountAsset(_ context.Context, acc uint64, assetIdx uint32) (accounttypes.AccountAsset, error) {
	return accounttypes.AccountAsset{
		AccountIndex: acc, AssetIndex: assetIdx,
		Balance: math.ZeroInt(), LockedBalance: math.ZeroInt(),
	}, nil
}
func (s *stubAccountKeeper) IterateAccounts(_ context.Context, _ func(accounttypes.Account) bool) error {
	return nil
}

// IterateAccountPositions yields s.pos when the seeded account matches.
// Skips empty positions (Position == 0) so behaviour matches the real
// keeper closely enough for the slice of tests that exercise the
// MaxPerpsMarketIndex full-scan code paths after the iterator refactor.
func (s *stubAccountKeeper) IterateAccountPositions(
	_ context.Context,
	accountIdx uint64,
	cb func(accounttypes.AccountPosition) bool,
) error {
	if s.pos.AccountIndex != accountIdx {
		return nil
	}
	if s.pos.BaseSize.IsNil() || s.pos.BaseSize.IsZero() {
		return nil
	}
	cb(s.pos)
	return nil
}

type stubMarketKeeper struct{ md markettypes.MarketDetails }

func (s stubMarketKeeper) GetMarket(_ context.Context, idx uint32) (markettypes.Market, error) {
	return markettypes.Market{MarketIndex: idx, MarketType: perptypes.MarketTypePerps}, nil
}
func (s stubMarketKeeper) GetMarketDetails(_ context.Context, _ uint32) (markettypes.MarketDetails, error) {
	return s.md, nil
}

type stubOracleKeeper struct {
	price oracletypes.OraclePrice
	err   error
}

func (s stubOracleKeeper) GetPrice(_ context.Context, _ uint32) (oracletypes.OraclePrice, error) {
	return s.price, s.err
}

func makeKeeper(t *testing.T, ak *stubAccountKeeper, mk stubMarketKeeper, ok stubOracleKeeper) (riskkeeper.Keeper, sdk.Context) {
	t.Helper()
	keys := storetypes.NewKVStoreKeys(risktypes.StoreKey)
	cdc := moduletestutil.MakeTestEncodingConfig().Codec
	cms := integration.CreateMultiStore(keys, log.NewTestLogger(t))
	ctx := sdk.NewContext(cms, cmtprototypes.Header{}, true, log.NewTestLogger(t))
	return riskkeeper.NewKeeper(
		cdc,
		runtime.NewKVStoreService(keys[risktypes.StoreKey]),
		"auth",
		ak, mk, ok,
	), ctx
}

// TestComputeRiskInfo_StalePriceFailsClosed ensures that non-zero positions
// with a missing or stale oracle price surface an explicit error rather than
// being silently skipped (audit Blocker risk-3).
func TestComputeRiskInfo_StalePriceFailsClosed(t *testing.T) {
	ak := stubAccountKeeper{
		acc: accounttypes.Account{AccountIndex: 1, Collateral: math.NewInt(1000)},
		pos: accounttypes.AccountPosition{
			AccountIndex: 1, MarketIndex: 0,
			BaseSize: math.NewInt(5), EntryQuote: math.NewInt(500),
			LastFundingRatePrefixSum: math.ZeroInt(), AllocatedMargin: math.ZeroInt(),
		},
	}
	mk := stubMarketKeeper{md: markettypes.MarketDetails{}}
	ok := stubOracleKeeper{err: oracletypes.ErrStalePrice}
	k, ctx := makeKeeper(t, &ak, mk, ok)

	_, err := k.ComputeRiskInfo(ctx, 1)
	require.ErrorIs(t, err, risktypes.ErrMissingPrice)
}

// TestComputeRiskInfo_ZeroMarkPriceRejected enforces the "non-zero mark"
// invariant on non-zero positions.
func TestComputeRiskInfo_ZeroMarkPriceRejected(t *testing.T) {
	ak := stubAccountKeeper{
		acc: accounttypes.Account{AccountIndex: 1, Collateral: math.NewInt(1000)},
		pos: accounttypes.AccountPosition{
			AccountIndex: 1, MarketIndex: 0,
			BaseSize: math.NewInt(5), EntryQuote: math.NewInt(500),
			LastFundingRatePrefixSum: math.ZeroInt(), AllocatedMargin: math.ZeroInt(),
		},
	}
	mk := stubMarketKeeper{md: markettypes.MarketDetails{}}
	ok := stubOracleKeeper{price: oracletypes.OraclePrice{MarkPrice: 0, IndexPrice: 100}}
	k, ctx := makeKeeper(t, &ak, mk, ok)

	_, err := k.ComputeRiskInfo(ctx, 1)
	require.ErrorIs(t, err, risktypes.ErrZeroMarkPrice)
}

// TestIsValidRiskChangeFrom_NoPreStateFailClosed verifies that an
// unhealthy post state without a pre-state snapshot fails closed
// (audit Blocker risk-3).
func TestIsValidRiskChangeFrom_NoPreStateFailClosed(t *testing.T) {
	// Position bought at 100_000 but mark is 10_000, so the account is
	// deeply under water → BANKRUPTCY in the post-state.
	ak := stubAccountKeeper{
		acc: accounttypes.Account{AccountIndex: 1, Collateral: math.NewInt(10)},
		pos: accounttypes.AccountPosition{
			AccountIndex: 1, MarketIndex: 0,
			BaseSize: math.NewInt(1_000_000), EntryQuote: math.NewInt(100_000_000_000),
			LastFundingRatePrefixSum: math.ZeroInt(), AllocatedMargin: math.ZeroInt(),
		},
	}
	mk := stubMarketKeeper{md: markettypes.MarketDetails{
		DefaultInitialMarginFraction: 1000, MaintenanceMarginFraction: 500,
		CloseOutMarginFraction: 250,
	}}
	ok := stubOracleKeeper{price: oracletypes.OraclePrice{MarkPrice: 10_000}}
	k, ctx := makeKeeper(t, &ak, mk, ok)

	ok2, err := k.IsValidRiskChangeFrom(ctx, 1, risktypes.PreRiskSnapshot{})
	require.NoError(t, err)
	require.False(t, ok2)
}

// TestGetPositionZeroPrice_LongMarkBased verifies the new mark-based
// zero-price formula for a long position. With M_i = 1% (100 bps),
// TAV = 50, MMR = 100, mark = 1000:
//
//	zeroPrice = mark * (1 - M_i * TAV / MMR)
//	          = 1000 * (1 - 0.01 * 50 / 100)
//	          = 1000 * (1 - 0.005) = 995
func TestGetPositionZeroPrice_LongMarkBased(t *testing.T) {
	ak := stubAccountKeeper{
		acc: accounttypes.Account{AccountIndex: 1, Collateral: math.NewInt(40)},
		pos: accounttypes.AccountPosition{
			AccountIndex: 1, MarketIndex: 0,
			BaseSize:                 math.NewInt(10), // long
			EntryQuote:               math.NewInt(-9_900),
			LastFundingRatePrefixSum: math.ZeroInt(),
			AllocatedMargin:          math.ZeroInt(),
			MarginMode:               perptypes.CrossMargin,
		},
	}
	// IM/MM/CM = 1% / 1% / 0.5%, mark = 1000.
	// notional = |10| * 1000 = 10_000, MMR = 10_000 * 0.01 = 100.
	// uPnL = 10*1000 - (-9_900) = 19_900? That's wrong; we want a
	// small TAV so the formula is intuitive. Adjust collateral so:
	// TAV = collateral + uPnL = 40 + (10_000 - 9_900) = 140? No, uPnL
	// = pos*mark - entryQuote = 10*1000 - (-9_900) = 19_900.
	// Actually entryQuote semantics: a long with cost basis 9_900 has
	// EntryQuote = -9_900 (paid quote out). So uPnL = pos*mark -
	// EntryQuote = 10_000 - (-9_900) = 19_900. We want TAV=50, so
	// reset entryQuote.
	// mark*pos - entry = 50 - collateral = 50 - 40 = 10  ⇒ entry =
	// 10_000 - 10 = 9_990.
	ak.pos.EntryQuote = math.NewInt(9_990)
	mk := stubMarketKeeper{md: markettypes.MarketDetails{
		DefaultInitialMarginFraction: 100, // 1%
		MaintenanceMarginFraction:    100, // 1%
		CloseOutMarginFraction:       50,  // 0.5%
	}}
	ok := stubOracleKeeper{price: oracletypes.OraclePrice{MarkPrice: 1000}}
	k, ctx := makeKeeper(t, &ak, mk, ok)

	zp, err := k.GetPositionZeroPrice(ctx, 1, 0)
	require.NoError(t, err)
	// mark = 1000, M_i = 100/10_000 = 0.01, TAV = 50, MMR = 100.
	// adjustment = 1000 * 100 * 50 / (100 * 10_000) = 5.
	// zeroPrice = 1000 - 5 = 995.
	require.Equal(t, uint32(995), zp,
		"long zero price must be mark*(1 - M*TAV/MMR)")
}

// TestGetPositionZeroPrice_ShortMarkBased mirrors the long test for a
// short position; the adjustment must ADD instead of subtract.
func TestGetPositionZeroPrice_ShortMarkBased(t *testing.T) {
	ak := stubAccountKeeper{
		acc: accounttypes.Account{AccountIndex: 1, Collateral: math.NewInt(40)},
		pos: accounttypes.AccountPosition{
			AccountIndex: 1, MarketIndex: 0,
			BaseSize:                 math.NewInt(-10), // short
			EntryQuote:               math.NewInt(-10_010),
			LastFundingRatePrefixSum: math.ZeroInt(),
			AllocatedMargin:          math.ZeroInt(),
			MarginMode:               perptypes.CrossMargin,
		},
	}
	mk := stubMarketKeeper{md: markettypes.MarketDetails{
		DefaultInitialMarginFraction: 100,
		MaintenanceMarginFraction:    100,
		CloseOutMarginFraction:       50,
	}}
	ok := stubOracleKeeper{price: oracletypes.OraclePrice{MarkPrice: 1000}}
	k, ctx := makeKeeper(t, &ak, mk, ok)

	zp, err := k.GetPositionZeroPrice(ctx, 1, 0)
	require.NoError(t, err)
	// uPnL = -10*1000 - (-10_010) = 10. TAV = 40 + 10 = 50? Wait —
	// collateral=40, uPnL=10, TAV=50, MMR=|−10|*1000*0.01 = 100.
	// adjustment = 1000 * 100 * 50 / (100 * 10_000) = 5.
	// zeroPrice short = mark + adjustment = 1005.
	require.Equal(t, uint32(1005), zp,
		"short zero price must be mark*(1 + M*TAV/MMR)")
}

// TestGetPositionZeroPrice_IsolatedUsesIsolatedTAV ensures the formula
// uses the isolated position's AllocatedMargin + uPnL (not cross
// aggregates) when the position is isolated.
func TestGetPositionZeroPrice_IsolatedUsesIsolatedTAV(t *testing.T) {
	ak := stubAccountKeeper{
		acc: accounttypes.Account{AccountIndex: 1, Collateral: math.NewInt(1_000_000)},
		pos: accounttypes.AccountPosition{
			AccountIndex: 1, MarketIndex: 0,
			BaseSize:                 math.NewInt(10),
			EntryQuote:               math.NewInt(9_990), // uPnL = 10
			LastFundingRatePrefixSum: math.ZeroInt(),
			AllocatedMargin:          math.NewInt(40), // isolated TAV = 50
			MarginMode:               perptypes.IsolatedMargin,
		},
	}
	mk := stubMarketKeeper{md: markettypes.MarketDetails{
		DefaultInitialMarginFraction: 100,
		MaintenanceMarginFraction:    100,
		CloseOutMarginFraction:       50,
	}}
	ok := stubOracleKeeper{price: oracletypes.OraclePrice{MarkPrice: 1000}}
	k, ctx := makeKeeper(t, &ak, mk, ok)

	zp, err := k.GetPositionZeroPrice(ctx, 1, 0)
	require.NoError(t, err)
	// TAV = AllocatedMargin + uPnL = 40 + 10 = 50; MMR = 100.
	// adjustment = 5 ⇒ zeroPrice = 1000 - 5 = 995.
	require.Equal(t, uint32(995), zp,
		"isolated zero price must use AllocatedMargin + uPnL, NOT cross collateral")
}

// TestComputeRiskInfo_IsolatedDoesNotPolluteCross ensures isolated
// positions are excluded from the cross aggregate. The previous
// implementation included isolated AllocatedMargin + uPnL in cross TAV
// without also accumulating IM/MM/CM, which let isolated profit
// silently inflate cross health.
func TestComputeRiskInfo_IsolatedDoesNotPolluteCross(t *testing.T) {
	ak := stubAccountKeeper{
		acc: accounttypes.Account{AccountIndex: 1, Collateral: math.NewInt(100)},
		pos: accounttypes.AccountPosition{
			AccountIndex: 1, MarketIndex: 0,
			BaseSize:                 math.NewInt(10),
			EntryQuote:               math.NewInt(9_000), // uPnL = 1_000 (large profit)
			LastFundingRatePrefixSum: math.ZeroInt(),
			AllocatedMargin:          math.NewInt(50),
			MarginMode:               perptypes.IsolatedMargin,
		},
	}
	mk := stubMarketKeeper{md: markettypes.MarketDetails{
		DefaultInitialMarginFraction: 100,
		MaintenanceMarginFraction:    100,
		CloseOutMarginFraction:       50,
	}}
	ok := stubOracleKeeper{price: oracletypes.OraclePrice{MarkPrice: 1000}}
	k, ctx := makeKeeper(t, &ak, mk, ok)

	ri, err := k.ComputeRiskInfo(ctx, 1)
	require.NoError(t, err)
	require.NotNil(t, ri.CrossRiskParameters)
	require.Equal(t, "100", ri.CrossRiskParameters.TotalAccountValue.String(),
		"cross TAV must equal cross collateral; isolated profit must not leak in")
	require.True(t, ri.CrossRiskParameters.MaintenanceMarginRequirement.IsZero(),
		"isolated MMR must not aggregate into cross MMR")
}

// TestGetIsolatedHealthStatus_PerMarket verifies isolated positions are
// classified independently from the cross aggregate.
func TestGetIsolatedHealthStatus_PerMarket(t *testing.T) {
	ak := stubAccountKeeper{
		acc: accounttypes.Account{AccountIndex: 1, Collateral: math.NewInt(1_000_000)},
		pos: accounttypes.AccountPosition{
			AccountIndex: 1, MarketIndex: 0,
			BaseSize:                 math.NewInt(10),
			EntryQuote:               math.NewInt(11_000), // uPnL = -1_000
			LastFundingRatePrefixSum: math.ZeroInt(),
			AllocatedMargin:          math.NewInt(500),
			MarginMode:               perptypes.IsolatedMargin,
		},
	}
	mk := stubMarketKeeper{md: markettypes.MarketDetails{
		DefaultInitialMarginFraction: 1000, // 10%
		MaintenanceMarginFraction:    500,  // 5%
		CloseOutMarginFraction:       250,  // 2.5%
	}}
	ok := stubOracleKeeper{price: oracletypes.OraclePrice{MarkPrice: 1000}}
	k, ctx := makeKeeper(t, &ak, mk, ok)

	// Cross is healthy (collateral plenty), but isolated has TAV =
	// AllocatedMargin + uPnL = 500 - 1000 = -500 → BANKRUPTCY.
	cross, err := k.GetHealthStatus(ctx, 1)
	require.NoError(t, err)
	require.Equal(t, perptypes.HealthHealthy, cross,
		"cross health must be HEALTHY; isolated must not affect it")

	iso, err := k.GetIsolatedHealthStatus(ctx, 1, 0)
	require.NoError(t, err)
	require.Equal(t, perptypes.HealthBankruptcy, iso,
		"isolated TAV<0 must classify as BANKRUPTCY independently")
}

// TestIsValidRiskChangeFrom_PreLiquidationRejectsMMRGrowth verifies
// the Lighter PRE rule. With pre.MMR snapshotted, a post-state with
// strictly larger MMR (i.e. account opened a new position) must be
// rejected even if post is still PRE_LIQUIDATION.
func TestIsValidRiskChangeFrom_PreLiquidationRejectsMMRGrowth(t *testing.T) {
	ak := stubAccountKeeper{
		acc: accounttypes.Account{AccountIndex: 1, Collateral: math.NewInt(1_000)},
		pos: accounttypes.AccountPosition{
			AccountIndex: 1, MarketIndex: 0,
			BaseSize:                 math.NewInt(20),
			EntryQuote:               math.NewInt(-19_900),
			LastFundingRatePrefixSum: math.ZeroInt(),
			AllocatedMargin:          math.ZeroInt(),
			MarginMode:               perptypes.CrossMargin,
		},
	}
	mk := stubMarketKeeper{md: markettypes.MarketDetails{
		DefaultInitialMarginFraction: 1000, // 10%
		MaintenanceMarginFraction:    500,  // 5%
		CloseOutMarginFraction:       250,  // 2.5%
	}}
	// notional = 20*1000 = 20_000; IM = 2_000; MM = 1_000; CM = 500.
	// uPnL = 20*1000 - (-19_900) = 39_900. Way too healthy.
	// Adjust entryQuote so TAV is between MM and IM (PRE):
	// We want collateral + uPnL = 1500 (between MM=1000 and IM=2000).
	// uPnL = 1500 - 1000 = 500 ⇒ entryQuote = pos*mark - uPnL =
	//     20_000 - 500 = 19_500 ⇒ stored as positive (we paid 19_500).
	// Wait sign: long means EntryQuote should be NEGATIVE in our
	// convention (you "spent" quote). Adjust: EntryQuote = -19_500 for
	// a long means uPnL = pos*mark - EntryQuote = 20_000 - (-19_500) =
	// 39_500. That doesn't help.
	// Use entry sign as +: EntryQuote = 19_500 ⇒ uPnL = 20_000 -
	// 19_500 = 500. TAV = 1_000 + 500 = 1_500 (PRE).
	ak.pos.EntryQuote = math.NewInt(19_500)
	ok := stubOracleKeeper{price: oracletypes.OraclePrice{MarkPrice: 1000}}
	k, ctx := makeKeeper(t, &ak, mk, ok)

	// Snapshot pre-state: PRE class.
	pre, err := k.SnapshotRisk(ctx, 1)
	require.NoError(t, err)
	// Sanity: pre is PRE.
	preStatus, err := k.GetHealthStatus(ctx, 1)
	require.NoError(t, err)
	require.Equal(t, perptypes.HealthPreLiquidation, preStatus)

	// Mutate post-state by growing the position. Same mark, so MMR
	// scales linearly with |position|. Increase position from 20 to
	// 30 → MMR grows from 1_000 to 1_500 → must be rejected.
	ak.pos.BaseSize = math.NewInt(30)
	// Keep TAV roughly in PRE range to isolate the MMR-growth signal.
	// TAV must still be < IM; IM grows from 2_000 to 3_000. Choose
	// uPnL such that TAV stays between MM(=1_500) and IM(=3_000).
	// collateral=1000, uPnL=1500 → TAV=2500 (PRE). uPnL=pos*mark -
	// entry ⇒ entry = 30_000 - 1500 = 28_500.
	ak.pos.EntryQuote = math.NewInt(28_500)
	ok2, err := k.IsValidRiskChangeFrom(ctx, 1, pre)
	require.NoError(t, err)
	require.False(t, ok2,
		"PRE → PRE with larger MMR must be rejected (Lighter no-size-up rule)")
}

// TestIsValidRiskChangeFrom_PreLiquidationAllowsReduceOnly verifies
// the inverse: if MMR shrinks while still in PRE, the change is
// accepted.
func TestIsValidRiskChangeFrom_PreLiquidationAllowsReduceOnly(t *testing.T) {
	ak := stubAccountKeeper{
		acc: accounttypes.Account{AccountIndex: 1, Collateral: math.NewInt(1_000)},
		pos: accounttypes.AccountPosition{
			AccountIndex: 1, MarketIndex: 0,
			BaseSize:                 math.NewInt(20),
			EntryQuote:               math.NewInt(19_500),
			LastFundingRatePrefixSum: math.ZeroInt(),
			AllocatedMargin:          math.ZeroInt(),
			MarginMode:               perptypes.CrossMargin,
		},
	}
	mk := stubMarketKeeper{md: markettypes.MarketDetails{
		DefaultInitialMarginFraction: 1000,
		MaintenanceMarginFraction:    500,
		CloseOutMarginFraction:       250,
	}}
	ok := stubOracleKeeper{price: oracletypes.OraclePrice{MarkPrice: 1000}}
	k, ctx := makeKeeper(t, &ak, mk, ok)

	pre, err := k.SnapshotRisk(ctx, 1)
	require.NoError(t, err)

	// Post: shrink position from 20 to 10. uPnL/collateral roughly
	// halves; TAV still > MMR; class stays at PRE or improves.
	ak.pos.BaseSize = math.NewInt(10)
	ak.pos.EntryQuote = math.NewInt(9_750)
	ok2, err := k.IsValidRiskChangeFrom(ctx, 1, pre)
	require.NoError(t, err)
	require.True(t, ok2, "shrinking position in PRE must be allowed")
}

// TestGetLiquidationRiskSnapshot_EmptyPositionShortCircuitsOracle
// pins the invariant that a snapshot for an empty position must not
// depend on oracle health: callers (gRPC GetPositionZeroPrice,
// Liquidate, Deleverage) only want the "no position" short-circuit
// and must not surface oracle errors. We intentionally configure the
// oracle to error so a leak would surface as a non-nil error.
func TestGetLiquidationRiskSnapshot_EmptyPositionShortCircuitsOracle(t *testing.T) {
	// Account holds NO position in market 0; stub returns the
	// zero-valued AccountPosition for that lookup.
	ak := stubAccountKeeper{
		acc: accounttypes.Account{AccountIndex: 1, Collateral: math.NewInt(1_000)},
		pos: accounttypes.AccountPosition{
			AccountIndex: 1, MarketIndex: 99, /* a different market */
			BaseSize: math.ZeroInt(), EntryQuote: math.ZeroInt(),
			LastFundingRatePrefixSum: math.ZeroInt(), AllocatedMargin: math.ZeroInt(),
		},
	}
	mk := stubMarketKeeper{md: markettypes.MarketDetails{}}
	// Oracle would fail if asked. The snapshot must not ask.
	ok := stubOracleKeeper{err: oracletypes.ErrStalePrice}
	k, ctx := makeKeeper(t, &ak, mk, ok)

	snap, err := k.GetLiquidationRiskSnapshot(ctx, 1, 0)
	require.NoError(t, err,
		"empty position must short-circuit before any oracle read")
	require.True(t, snap.Position.BaseSize.IsZero())
	require.Equal(t, uint32(0), snap.MarkPrice)
	require.Equal(t, uint32(0), snap.ZeroPrice)

	// gRPC entry point must mirror the snapshot's short-circuit.
	zp, err := k.GetPositionZeroPrice(ctx, 1, 0)
	require.NoError(t, err,
		"GetPositionZeroPrice must keep the empty-position semantics: 0 regardless of oracle health")
	require.Equal(t, uint32(0), zp)
}
