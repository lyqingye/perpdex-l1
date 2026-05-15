// Tests for the (market, account) secondary index `FlagsByMarket`
// that keeper bots use to enumerate every flagged account on a
// single market without a full-table scan of the primary `Flags`
// map.
//
// The invariant verified across every test in this file is:
// `FlagsByMarket` is updated in lock-step with `Flags`, both on
// write (EndBlocker / Liquidate paths) and on clear (HEALTHY sweep,
// market-exit), and on genesis import. A divergence between the
// two collections would silently make the secondary index lie about
// what is currently flagged.
package tests

import (
	"sort"
	"testing"

	"cosmossdk.io/collections"
	"cosmossdk.io/math"

	"github.com/stretchr/testify/require"

	perptypes "github.com/perpdex/perpdex-l1/types"
	accounttypes "github.com/perpdex/perpdex-l1/x/account/types"
	liqkeeper "github.com/perpdex/perpdex-l1/x/liquidation/keeper"
	liqtypes "github.com/perpdex/perpdex-l1/x/liquidation/types"
	markettypes "github.com/perpdex/perpdex-l1/x/market/types"
)

// TestFlagsByMarket_PopulatedOnEndBlocker drives EndBlocker against
// an unhealthy account and asserts that the (market, account)
// secondary index records the same flag the primary `Flags` map
// just wrote. Without this lock-step write, keeper bots scanning a
// market would not see the freshly-flagged account.
func TestFlagsByMarket_PopulatedOnEndBlocker(t *testing.T) {
	ak := newStubAccount()
	ak.accounts[100] = accounttypes.Account{
		AccountIndex: 100, Collateral: math.ZeroInt(),
	}
	ak.pos[[2]uint64{100, 7}] = accounttypes.AccountPosition{
		AccountIndex: 100, MarketIndex: 7,
		BaseSize: math.NewInt(3), EntryQuote: math.NewInt(-30),
		LastFundingRatePrefixSum: math.ZeroInt(),
		AllocatedMargin:          math.ZeroInt(),
	}
	rk := newStubRisk()
	rk.status = perptypes.HealthPartialLiquidation
	tk := &stubTrade{}
	matchk := newStubMatching()
	k, ctx := newKeeper(t, ak, rk, tk, matchk)

	require.NoError(t, k.EndBlocker(ctx))

	primaryHas, err := k.Flags.Has(ctx, collections.Join[uint64, uint32](100, 7))
	require.NoError(t, err)
	require.True(t, primaryHas, "primary Flags must record the new flag")

	secondaryHas, err := k.FlagsByMarket.Has(ctx, collections.Join[uint32, uint64](7, 100))
	require.NoError(t, err)
	require.True(t, secondaryHas,
		"FlagsByMarket secondary index must mirror the primary write")
}

// TestFlagsByMarket_ClearedOnHealthyRecovery seeds a flag, recovers
// the account to HEALTHY across an EndBlocker run, and verifies
// that BOTH indices drop the (account, market) pair. The
// `clearCrossFlags` defensive sweep must go through the helper, not
// touch `Flags` directly.
func TestFlagsByMarket_ClearedOnHealthyRecovery(t *testing.T) {
	ak := newStubAccount()
	ak.accounts[100] = accounttypes.Account{
		AccountIndex: 100, Collateral: math.ZeroInt(),
	}
	ak.pos[[2]uint64{100, 7}] = accounttypes.AccountPosition{
		AccountIndex: 100, MarketIndex: 7,
		BaseSize: math.NewInt(3), EntryQuote: math.NewInt(-30),
		LastFundingRatePrefixSum: math.ZeroInt(),
		AllocatedMargin:          math.ZeroInt(),
	}
	rk := newStubRisk()
	rk.status = perptypes.HealthPartialLiquidation
	tk := &stubTrade{}
	matchk := newStubMatching()
	k, ctx := newKeeper(t, ak, rk, tk, matchk)

	require.NoError(t, k.EndBlocker(ctx),
		"seed run: flag the account at (100, 7)")
	secondaryHas, err := k.FlagsByMarket.Has(ctx, collections.Join[uint32, uint64](7, 100))
	require.NoError(t, err)
	require.True(t, secondaryHas, "precondition: secondary index seeded")

	rk.status = perptypes.HealthHealthy
	require.NoError(t, k.EndBlocker(ctx),
		"recovery run: account flips back to HEALTHY")

	primaryHas, err := k.Flags.Has(ctx, collections.Join[uint64, uint32](100, 7))
	require.NoError(t, err)
	require.False(t, primaryHas, "primary Flags must drop the recovered flag")

	secondaryHas, err = k.FlagsByMarket.Has(ctx, collections.Join[uint32, uint64](7, 100))
	require.NoError(t, err)
	require.False(t, secondaryHas,
		"FlagsByMarket secondary index must drop the recovered flag in lock-step")
}

// TestFlagsByMarket_ClearedOnExitPosition forces a market exit
// against a flagged victim and asserts the exit path drops both
// indices. The market-expiry path is the only non-EndBlocker /
// non-genesis caller of the flag mutators, so it has its own test.
func TestFlagsByMarket_ClearedOnExitPosition(t *testing.T) {
	ak := newStubAccount()
	ak.accounts[100] = accounttypes.Account{
		AccountIndex: 100, Collateral: math.NewInt(1000),
	}
	ak.accounts[perptypes.InsuranceFundOperatorAccountIdx] = accounttypes.Account{
		AccountIndex: perptypes.InsuranceFundOperatorAccountIdx,
		AccountType:  perptypes.InsuranceFundAccountType,
		Collateral:   math.NewInt(1_000_000),
	}
	ak.pos[[2]uint64{100, 9}] = accounttypes.AccountPosition{
		AccountIndex: 100, MarketIndex: 9,
		BaseSize: math.NewInt(2), EntryQuote: math.NewInt(-200),
		LastFundingRatePrefixSum: math.ZeroInt(),
		AllocatedMargin:          math.ZeroInt(),
	}
	rk := newStubRisk()
	rk.status = perptypes.HealthPartialLiquidation
	rk.marks = map[uint32]uint32{9: 100}
	rk.mds = map[uint32]markettypes.MarketDetails{
		9: {MarketIndex: 9, MarkPrice: 100, LastMarkPriceRefreshTimestamp: 1},
	}
	tk := &stubTrade{}
	matchk := newStubMatching()
	k, ctx := newKeeper(t, ak, rk, tk, matchk)

	require.NoError(t, k.EndBlocker(ctx),
		"seed: flag the victim on market 9")
	has, err := k.FlagsByMarket.Has(ctx, collections.Join[uint32, uint64](9, 100))
	require.NoError(t, err)
	require.True(t, has, "precondition: secondary index seeded")

	require.NoError(t, k.ApplyExitPosition(ctx, 9))

	primaryHas, err := k.Flags.Has(ctx, collections.Join[uint64, uint32](100, 9))
	require.NoError(t, err)
	require.False(t, primaryHas, "primary Flags must drop the flag on market exit")
	secondaryHas, err := k.FlagsByMarket.Has(ctx, collections.Join[uint32, uint64](9, 100))
	require.NoError(t, err)
	require.False(t, secondaryHas,
		"FlagsByMarket secondary index must drop the flag in lock-step on market exit")
}

// TestFlagsByMarket_GenesisRebuildsIndex imports a genesis state
// that holds the primary flag list and asserts the secondary index
// is rebuilt from the import. Genesis is the only path where the
// secondary index is created without an EndBlocker / Liquidate
// mutation; a regression here would silently corrupt the index on
// chain upgrade or simulation seeding.
func TestFlagsByMarket_GenesisRebuildsIndex(t *testing.T) {
	ak := newStubAccount()
	rk := newStubRisk()
	tk := &stubTrade{}
	matchk := newStubMatching()
	k, ctx := newKeeper(t, ak, rk, tk, matchk)

	flags := []liqtypes.LiquidationFlag{
		{AccountIndex: 100, MarketIndex: 1, FlaggedAtBlock: 10, FlaggedAtTime: 1_700_000},
		{AccountIndex: 100, MarketIndex: 2, FlaggedAtBlock: 11, FlaggedAtTime: 1_700_001},
		{AccountIndex: 200, MarketIndex: 1, FlaggedAtBlock: 12, FlaggedAtTime: 1_700_002},
	}
	require.NoError(t, k.InitGenesis(ctx, liqtypes.GenesisState{
		Params: liqtypes.DefaultParams(),
		Flags:  flags,
	}))

	for _, f := range flags {
		has, err := k.FlagsByMarket.Has(ctx, collections.Join[uint32, uint64](f.MarketIndex, f.AccountIndex))
		require.NoError(t, err)
		require.True(t, has,
			"FlagsByMarket must contain (market=%d, account=%d) after genesis import",
			f.MarketIndex, f.AccountIndex)
	}

	rng := collections.NewPrefixedPairRange[uint32, uint64](1)
	iter, err := k.FlagsByMarket.Iterate(ctx, rng)
	require.NoError(t, err)
	defer iter.Close()
	gotMarket1 := []uint64{}
	for ; iter.Valid(); iter.Next() {
		key, err := iter.Key()
		require.NoError(t, err)
		gotMarket1 = append(gotMarket1, key.K2())
	}
	sort.Slice(gotMarket1, func(i, j int) bool { return gotMarket1[i] < gotMarket1[j] })
	require.Equal(t, []uint64{100, 200}, gotMarket1,
		"market=1 prefix scan must yield both flagged accounts")
}

// TestQuery_LiquidationFlagsByMarket exercises the gRPC surface of
// the new secondary index. The querier MUST:
//
//   - Return every flag set on the requested market and ONLY those
//     flags (no leakage from sibling markets sharing an account).
//   - Pull the full LiquidationFlag value from the primary `Flags`
//     map so the response carries (flagged_at_block, flagged_at_time)
//     metadata, not just the (market, account) tuple.
//   - Reject a nil request rather than panicking.
func TestQuery_LiquidationFlagsByMarket(t *testing.T) {
	ak := newStubAccount()
	rk := newStubRisk()
	tk := &stubTrade{}
	matchk := newStubMatching()
	k, ctx := newKeeper(t, ak, rk, tk, matchk)
	q := liqkeeper.NewQuerier(k)

	flags := []liqtypes.LiquidationFlag{
		{AccountIndex: 100, MarketIndex: 1, FlaggedAtBlock: 10, FlaggedAtTime: 1_700_000},
		{AccountIndex: 200, MarketIndex: 1, FlaggedAtBlock: 12, FlaggedAtTime: 1_700_002},
		{AccountIndex: 100, MarketIndex: 2, FlaggedAtBlock: 11, FlaggedAtTime: 1_700_001},
	}
	require.NoError(t, k.InitGenesis(ctx, liqtypes.GenesisState{
		Params: liqtypes.DefaultParams(),
		Flags:  flags,
	}))

	resp, err := q.LiquidationFlagsByMarket(ctx,
		&liqtypes.QueryLiquidationFlagsByMarketRequest{MarketIndex: 1},
	)
	require.NoError(t, err)
	got := resp.Flags
	sort.Slice(got, func(i, j int) bool { return got[i].AccountIndex < got[j].AccountIndex })
	require.Equal(t, []liqtypes.LiquidationFlag{
		{AccountIndex: 100, MarketIndex: 1, FlaggedAtBlock: 10, FlaggedAtTime: 1_700_000},
		{AccountIndex: 200, MarketIndex: 1, FlaggedAtBlock: 12, FlaggedAtTime: 1_700_002},
	}, got, "market=1 query must yield both flagged accounts with full metadata")

	resp2, err := q.LiquidationFlagsByMarket(ctx,
		&liqtypes.QueryLiquidationFlagsByMarketRequest{MarketIndex: 2},
	)
	require.NoError(t, err)
	require.Equal(t, []liqtypes.LiquidationFlag{
		{AccountIndex: 100, MarketIndex: 2, FlaggedAtBlock: 11, FlaggedAtTime: 1_700_001},
	}, resp2.Flags, "market=2 query must not leak market=1's flags")

	resp3, err := q.LiquidationFlagsByMarket(ctx,
		&liqtypes.QueryLiquidationFlagsByMarketRequest{MarketIndex: 999},
	)
	require.NoError(t, err)
	require.Empty(t, resp3.Flags,
		"unknown market must yield an empty (not nil) flag list")

	_, err = q.LiquidationFlagsByMarket(ctx, nil)
	require.Error(t, err, "nil request must surface InvalidArgument")
}
