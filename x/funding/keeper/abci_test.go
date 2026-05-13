package keeper_test

import (
	"context"
	"errors"
	"testing"
	"time"

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
	fundingkeeper "github.com/perpdex/perpdex-l1/x/funding/keeper"
	fundingtypes "github.com/perpdex/perpdex-l1/x/funding/types"
	markettypes "github.com/perpdex/perpdex-l1/x/market/types"
	oracletypes "github.com/perpdex/perpdex-l1/x/oracle/types"
)

type stubMarket struct {
	markets map[uint32]markettypes.Market
	details map[uint32]markettypes.MarketDetails
	sets    int
}

func (s *stubMarket) GetMarket(_ context.Context, idx uint32) (markettypes.Market, error) {
	if m, ok := s.markets[idx]; ok {
		return m, nil
	}
	return markettypes.Market{}, errors.New("not found")
}
func (s *stubMarket) GetMarketDetails(_ context.Context, idx uint32) (markettypes.MarketDetails, error) {
	if d, ok := s.details[idx]; ok {
		return d, nil
	}
	return markettypes.MarketDetails{
		MarketIndex:          idx,
		FundingRatePrefixSum: math.ZeroInt(),
		AggregatePremiumSum:  math.ZeroInt(),
	}, nil
}
func (s *stubMarket) SetMarketDetails(_ context.Context, d markettypes.MarketDetails) error {
	if s.details == nil {
		s.details = map[uint32]markettypes.MarketDetails{}
	}
	s.details[d.MarketIndex] = d
	s.sets++
	return nil
}
func (s *stubMarket) IterateMarkets(_ context.Context, cb func(markettypes.Market) bool) error {
	for _, m := range s.markets {
		if cb(m) {
			return nil
		}
	}
	return nil
}

// stubBook fakes the orderbook for funding sampler tests. A zero bid /
// ask value represents insufficient depth on that side.
type stubBook struct {
	bid, ask uint32
}

func (s stubBook) BestBidAsk(_ context.Context, _ uint32) (uint32, uint32, error) {
	return s.bid, s.ask, nil
}
func (s stubBook) ComputeImpactPrice(_ context.Context, _ uint32, isAsk bool) (uint32, error) {
	if isAsk {
		return s.ask, nil
	}
	return s.bid, nil
}

type stubOracle struct {
	price  oracletypes.OraclePrice
	err    error
	prices map[uint32]oracletypes.OraclePrice
	errs   map[uint32]error
}

func (s stubOracle) GetPrice(_ context.Context, marketIdx uint32) (oracletypes.OraclePrice, error) {
	if err, ok := s.errs[marketIdx]; ok && err != nil {
		return oracletypes.OraclePrice{}, err
	}
	if p, ok := s.prices[marketIdx]; ok {
		return p, nil
	}
	if s.err != nil {
		return oracletypes.OraclePrice{}, s.err
	}
	return s.price, nil
}

type stubAccount struct{}

func (stubAccount) GetPosition(_ context.Context, acc uint64, mkt uint32) (accounttypes.AccountPosition, error) {
	return accounttypes.AccountPosition{
		AccountIndex: acc, MarketIndex: mkt,
		BaseSize: math.ZeroInt(), EntryQuote: math.ZeroInt(),
		LastFundingRatePrefixSum: math.ZeroInt(), AllocatedMargin: math.ZeroInt(),
	}, nil
}

// UpdatePosition is a no-op closure runner; the funding keeper's
// SettlePositionFunding now dispatches every write through this
// surface, but the stub doesn't persist anything (the suite cares
// only about the prefix-sum / mark-price computations on the market
// side).
func (s stubAccount) UpdatePosition(
	ctx context.Context,
	accIdx uint64,
	marketIdx uint32,
	mut func(*accounttypes.AccountPosition) error,
) (accounttypes.AccountPosition, error) {
	pos, err := s.GetPosition(ctx, accIdx, marketIdx)
	if err != nil {
		return accounttypes.AccountPosition{}, err
	}
	pos.AccountIndex = accIdx
	pos.MarketIndex = marketIdx
	if err := mut(&pos); err != nil {
		return accounttypes.AccountPosition{}, err
	}
	return pos, nil
}

type statefulAccount struct {
	positions map[[2]uint64]accounttypes.AccountPosition
}

func newStatefulAccount() *statefulAccount {
	return &statefulAccount{positions: map[[2]uint64]accounttypes.AccountPosition{}}
}

func (s *statefulAccount) GetPosition(_ context.Context, acc uint64, mkt uint32) (accounttypes.AccountPosition, error) {
	key := [2]uint64{acc, uint64(mkt)}
	if p, ok := s.positions[key]; ok {
		return p, nil
	}
	return accounttypes.AccountPosition{
		AccountIndex: acc, MarketIndex: mkt,
		BaseSize: math.ZeroInt(), EntryQuote: math.ZeroInt(),
		LastFundingRatePrefixSum: math.ZeroInt(), AllocatedMargin: math.ZeroInt(),
	}, nil
}

// SetPosition is a stub-only fixture helper used by this suite to
// preload positions. The production AccountKeeper interface does not
// expose a generic position setter.
func (s *statefulAccount) SetPosition(_ context.Context, p accounttypes.AccountPosition) error {
	key := [2]uint64{p.AccountIndex, uint64(p.MarketIndex)}
	s.positions[key] = p
	return nil
}

// UpdatePosition mirrors the real keeper's RMW closure surface so
// SettlePositionFunding's funding-payment write hits this stub.
func (s *statefulAccount) UpdatePosition(
	ctx context.Context,
	accIdx uint64,
	marketIdx uint32,
	mut func(*accounttypes.AccountPosition) error,
) (accounttypes.AccountPosition, error) {
	pos, err := s.GetPosition(ctx, accIdx, marketIdx)
	if err != nil {
		return accounttypes.AccountPosition{}, err
	}
	pos.AccountIndex = accIdx
	pos.MarketIndex = marketIdx
	if err := mut(&pos); err != nil {
		return accounttypes.AccountPosition{}, err
	}
	if err := s.SetPosition(ctx, pos); err != nil {
		return accounttypes.AccountPosition{}, err
	}
	return pos, nil
}

func newFundingKeeper(t *testing.T, mk *stubMarket, ok fundingtypes.OracleKeeper, bk stubBook) (fundingkeeper.Keeper, sdk.Context) {
	t.Helper()
	keys := storetypes.NewKVStoreKeys(fundingtypes.StoreKey)
	cdc := moduletestutil.MakeTestEncodingConfig().Codec
	cms := integration.CreateMultiStore(keys, log.NewTestLogger(t))
	hdr := cmtprototypes.Header{Time: time.Unix(0, 1_700_000_000_000_000_000)}
	ctx := sdk.NewContext(cms, hdr, true, log.NewTestLogger(t))
	k := fundingkeeper.NewKeeper(
		cdc,
		runtime.NewKVStoreService(keys[fundingtypes.StoreKey]),
		"auth",
		mk, ok, bk, stubAccount{},
	)
	require.NoError(t, k.Params.Set(ctx, fundingtypes.DefaultParams()))
	// Default: pretend funding has just settled so the begin-blocker's
	// `LastFundingRoundTimestamp == 0` short-circuit does not fire and tests
	// can drive `processMarketSample` in isolation. Tests that need to
	// exercise the settle path advance ctx time past `FundingPeriodMs`.
	require.NoError(t, k.Metadata.Set(ctx, fundingtypes.FundingMetadata{
		LastFundingRoundTimestamp: ctx.BlockTime().UnixMilli(),
	}))
	return k, ctx
}

func newFundingKeeperWithAccount(
	t *testing.T,
	mk *stubMarket,
	ok stubOracle,
	bk stubBook,
	ak *statefulAccount,
) (fundingkeeper.Keeper, sdk.Context) {
	t.Helper()
	keys := storetypes.NewKVStoreKeys(fundingtypes.StoreKey)
	cdc := moduletestutil.MakeTestEncodingConfig().Codec
	cms := integration.CreateMultiStore(keys, log.NewTestLogger(t))
	hdr := cmtprototypes.Header{Time: time.Unix(0, 1_700_000_000_000_000_000)}
	ctx := sdk.NewContext(cms, hdr, true, log.NewTestLogger(t))
	k := fundingkeeper.NewKeeper(
		cdc,
		runtime.NewKVStoreService(keys[fundingtypes.StoreKey]),
		"auth",
		mk, ok, bk, ak,
	)
	require.NoError(t, k.Params.Set(ctx, fundingtypes.DefaultParams()))
	require.NoError(t, k.Metadata.Set(ctx, fundingtypes.FundingMetadata{
		LastFundingRoundTimestamp: ctx.BlockTime().UnixMilli(),
	}))
	return k, ctx
}

// TestSettlePositionFunding_ZeroPositionSnapshotsCurrentPrefix ensures
// a fresh or fully closed position snapshots the current funding
// prefix so it does not inherit any prefix accumulated before it
// opened.
func TestSettlePositionFunding_ZeroPositionSnapshotsCurrentPrefix(t *testing.T) {
	const (
		accountIndex = uint64(7)
		marketIndex  = uint32(1)
	)

	mk := &stubMarket{
		markets: map[uint32]markettypes.Market{
			marketIndex: {MarketIndex: marketIndex, MarketType: perptypes.MarketTypePerps, Status: perptypes.MarketStatusActive},
		},
		details: map[uint32]markettypes.MarketDetails{
			marketIndex: {
				MarketIndex:          marketIndex,
				FundingRatePrefixSum: math.NewInt(100_000_000),
				AggregatePremiumSum:  math.ZeroInt(),
			},
		},
	}
	ak := newStatefulAccount()
	k, ctx := newFundingKeeperWithAccount(
		t,
		mk,
		stubOracle{price: oracletypes.OraclePrice{IndexPrice: 49_500, MarkPrice: 50_000}},
		stubBook{},
		ak,
	)

	require.NoError(t, k.SettlePositionFunding(ctx, accountIndex, marketIndex))
	key := [2]uint64{accountIndex, uint64(marketIndex)}
	snapshotted := ak.positions[key]
	require.True(t, snapshotted.BaseSize.IsZero())
	require.EqualValues(t, 100_000_000, snapshotted.LastFundingRatePrefixSum.Int64())

	// Simulate ApplyPerpsMatching opening a new position after the
	// zero-size settle above, then advance the market prefix by only
	// 20_000_000. The next funding settlement must charge that new
	// delta only, not the full 120_000_000 prefix.
	snapshotted.BaseSize = math.NewInt(1_000_000)
	snapshotted.EntryQuote = math.ZeroInt()
	ak.positions[key] = snapshotted
	d := mk.details[marketIndex]
	d.FundingRatePrefixSum = math.NewInt(120_000_000)
	mk.details[marketIndex] = d

	require.NoError(t, k.SettlePositionFunding(ctx, accountIndex, marketIndex))
	settled := ak.positions[key]
	require.EqualValues(t, 20_000_000, settled.EntryQuote.Int64())
	require.EqualValues(t, 120_000_000, settled.LastFundingRatePrefixSum.Int64())
}

func TestProcessMarketSample_StaleOracleSkipsSample(t *testing.T) {
	mk := &stubMarket{
		markets: map[uint32]markettypes.Market{
			1: {MarketIndex: 1, MarketType: perptypes.MarketTypePerps, Status: perptypes.MarketStatusActive},
		},
		details: map[uint32]markettypes.MarketDetails{
			1: {
				MarketIndex:          1,
				IndexPrice:           49_500,
				MarkPrice:            50_000,
				FundingRatePrefixSum: math.ZeroInt(),
				AggregatePremiumSum:  math.ZeroInt(),
			},
		},
	}
	k, ctx := newFundingKeeper(
		t,
		mk,
		stubOracle{err: oracletypes.ErrStalePrice.Wrap("stale fixture")},
		stubBook{bid: 49_999, ask: 50_001},
	)

	require.NoError(t, k.BeginBlocker(ctx))
	got := mk.details[1]
	require.EqualValues(t, 0, got.TotalPremiumSamples)
	require.True(t, got.AggregatePremiumSum.IsZero())
	require.EqualValues(t, 0, got.LastPremiumSampleTimestamp, "stale oracle must not throttle retries")
}

// TestSettleMarket_UsesCachedMarkAndIgnoresOracleStaleness verifies that
// settlement does NOT depend on a fresh oracle read: rate computation only
// needs the accumulated samples + InterestRate + clamps, and the mark price
// for the prefix-sum increment is read from MarketDetails.MarkPrice (cached
// during the most recent successful processMarketSample). A stale oracle at
// settle time MUST NOT abort the round nor preserve cross-window data.
func TestSettleMarket_UsesCachedMarkAndIgnoresOracleStaleness(t *testing.T) {
	oldRoundTs := int64(1_699_996_399_999)
	mk := &stubMarket{
		markets: map[uint32]markettypes.Market{
			1: {MarketIndex: 1, MarketType: perptypes.MarketTypePerps, Status: perptypes.MarketStatusActive},
		},
		details: map[uint32]markettypes.MarketDetails{
			1: {
				MarketIndex:          1,
				MarkPrice:            50_000, // cached by a previous successful sample
				IndexPrice:           49_500,
				FundingRatePrefixSum: math.ZeroInt(),
				AggregatePremiumSum:  math.NewInt(10_101 * 60),
				TotalPremiumSamples:  60,
				FundingClampSmall:    500,
				FundingClampBig:      40_000,
			},
		},
	}
	k, ctx := newFundingKeeper(
		t,
		mk,
		stubOracle{err: oracletypes.ErrStalePrice.Wrap("stale fixture")},
		stubBook{},
	)
	require.NoError(t, k.Metadata.Set(ctx, fundingtypes.FundingMetadata{
		LastFundingRoundTimestamp: oldRoundTs,
	}))

	require.NoError(t, k.BeginBlocker(ctx), "stale oracle must not abort begin-block")
	got := mk.details[1]
	require.EqualValues(t, 60_000_000, got.FundingRatePrefixSum.Int64(),
		"settle must proceed with cached mark even when oracle is stale")
	require.True(t, got.AggregatePremiumSum.IsZero(), "samples must be cleared after settle")
	require.EqualValues(t, 0, got.TotalPremiumSamples)
	meta, metaErr := k.Metadata.Get(ctx)
	require.NoError(t, metaErr)
	require.EqualValues(t, ctx.BlockTime().UnixMilli(), meta.LastFundingRoundTimestamp)
}

// TestSettleAllMarkets_AllMarketsSettleIndependentlyOfOracle covers the
// per-round behaviour for two markets: even when one market's oracle is
// stale at settle time, both still close the round using their cached mark
// prices and clear their samples for the next window.
func TestSettleAllMarkets_AllMarketsSettleIndependentlyOfOracle(t *testing.T) {
	oldRoundTs := int64(1_699_996_399_999)
	mk := &stubMarket{
		markets: map[uint32]markettypes.Market{
			1: {MarketIndex: 1, MarketType: perptypes.MarketTypePerps, Status: perptypes.MarketStatusActive},
			2: {MarketIndex: 2, MarketType: perptypes.MarketTypePerps, Status: perptypes.MarketStatusActive},
		},
		details: map[uint32]markettypes.MarketDetails{
			1: {
				MarketIndex:          1,
				MarkPrice:            50_000,
				IndexPrice:           49_500,
				FundingRatePrefixSum: math.ZeroInt(),
				AggregatePremiumSum:  math.NewInt(10_101 * 60),
				TotalPremiumSamples:  60,
				FundingClampSmall:    500,
				FundingClampBig:      40_000,
			},
			2: {
				MarketIndex:          2,
				MarkPrice:            50_000, // cached from a prior successful sample
				IndexPrice:           49_500,
				FundingRatePrefixSum: math.ZeroInt(),
				AggregatePremiumSum:  math.NewInt(20_000 * 60),
				TotalPremiumSamples:  60,
				FundingClampSmall:    500,
				FundingClampBig:      40_000,
			},
		},
	}
	k, ctx := newFundingKeeper(
		t,
		mk,
		stubOracle{
			prices: map[uint32]oracletypes.OraclePrice{
				1: {MarketIndex: 1, IndexPrice: 49_500, MarkPrice: 50_000},
			},
			errs: map[uint32]error{
				2: oracletypes.ErrStalePrice.Wrap("stale fixture"),
			},
		},
		stubBook{},
	)
	require.NoError(t, k.Metadata.Set(ctx, fundingtypes.FundingMetadata{
		LastFundingRoundTimestamp: oldRoundTs,
	}))

	require.NoError(t, k.BeginBlocker(ctx))

	// Market 1: oracle fresh. refreshMarkPrice runs before settle and
	// re-derives MarkPrice. impact=0 freezes premium_raw to 0; the
	// first refresh reseeds EMA to 0; price_1 = idx = 49_500. impact=0
	// triggers the mean fallback ⇒ mean(price_1, price_2) =
	// mean(49500, 50000) = 49_750. inc = 49_750 * 1200 = 59_700_000.
	settled := mk.details[1]
	require.EqualValues(t, 59_700_000, settled.FundingRatePrefixSum.Int64())
	require.True(t, settled.AggregatePremiumSum.IsZero())
	require.EqualValues(t, 0, settled.TotalPremiumSamples)

	// Market 2: oracle stale at settle but cached MarkPrice is still 50_000;
	// settle must complete using the cached mark and clear the window.
	settled2 := mk.details[2]
	avg2 := int64(20_000)
	corr2 := int64(-500) // clamp(0 - 20_000, ±500) = -500
	rate2 := (avg2 + corr2) / 8
	expected2 := int64(settled2.MarkPrice) * rate2
	require.EqualValues(t, expected2, settled2.FundingRatePrefixSum.Int64(),
		"market 2 must settle using cached MarkPrice even when oracle is stale")
	require.True(t, settled2.AggregatePremiumSum.IsZero())
	require.EqualValues(t, 0, settled2.TotalPremiumSamples)

	meta, metaErr := k.Metadata.Get(ctx)
	require.NoError(t, metaErr)
	require.EqualValues(t, ctx.BlockTime().UnixMilli(), meta.LastFundingRoundTimestamp)
}

func TestMarketFundingRateQuery_ReturnsGlobalFundingRoundTimestamp(t *testing.T) {
	mk := &stubMarket{
		markets: map[uint32]markettypes.Market{
			1: {MarketIndex: 1, MarketType: perptypes.MarketTypePerps, Status: perptypes.MarketStatusActive},
		},
		details: map[uint32]markettypes.MarketDetails{
			1: {
				MarketIndex:                1,
				FundingRatePrefixSum:       math.NewInt(123),
				AggregatePremiumSum:        math.ZeroInt(),
				LastPremiumSampleTimestamp: 456,
			},
		},
	}
	k, ctx := newFundingKeeper(
		t,
		mk,
		stubOracle{price: oracletypes.OraclePrice{IndexPrice: 49_500, MarkPrice: 50_000}},
		stubBook{},
	)
	require.NoError(t, k.Metadata.Set(ctx, fundingtypes.FundingMetadata{
		LastFundingRoundTimestamp: 789,
	}))

	resp, err := fundingkeeper.NewQuerier(k).MarketFundingRate(ctx, &fundingtypes.QueryMarketFundingRateRequest{MarketIndex: 1})
	require.NoError(t, err)
	require.EqualValues(t, 123, resp.PrefixSum.Int64())
	require.EqualValues(t, 789, resp.LastSettledAt)
}

// TestProcessMarketSample_OneSidedSkipsSample verifies that when only one
// side of the book can absorb ImpactUSDCAmount of quote we skip the sample
// (rather than feeding ImpactBid=0 / ImpactAsk=0 into the formula, which
// would otherwise peg the premium near -100%).
func TestProcessMarketSample_OneSidedSkipsSample(t *testing.T) {
	mk := &stubMarket{
		markets: map[uint32]markettypes.Market{
			1: {MarketIndex: 1, MarketType: perptypes.MarketTypePerps, Status: perptypes.MarketStatusActive},
		},
		details: map[uint32]markettypes.MarketDetails{
			1: {
				MarketIndex:          1,
				ImpactPrice:          999, // stale
				AggregatePremiumSum:  math.NewInt(42),
				TotalPremiumSamples:  1,
				FundingRatePrefixSum: math.ZeroInt(),
			},
		},
	}
	ok := stubOracle{price: oracletypes.OraclePrice{IndexPrice: 100, MarkPrice: 100}}
	bk := stubBook{ask: 110} // ask depth only

	k, ctx := newFundingKeeper(t, mk, ok, bk)
	require.NoError(t, k.BeginBlocker(ctx))
	got := mk.details[1]
	require.EqualValues(t, 0, got.ImpactPrice, "stale impact mid must be cleared")
	require.EqualValues(t, 0, got.ImpactBidPrice)
	require.EqualValues(t, 110, got.ImpactAskPrice)
	require.EqualValues(t, 42, got.AggregatePremiumSum.Int64(), "premium sum must not move when a side has no depth")
	require.EqualValues(t, 1, got.TotalPremiumSamples, "sample count must not advance")
}

// TestProcessMarketSample_Premium drives the premium formula directly:
//
//	premium_t = ( max(0, IB - idx) - max(0, idx - IA) ) * TICK / idx
//
// With IB=49999, IA=50001 and idx=49500 we expect
// `(49999-49500) * 1e6 / 49500 = 499*1e6/49500 = 10080`.
func TestProcessMarketSample_Premium(t *testing.T) {
	mk := &stubMarket{
		markets: map[uint32]markettypes.Market{
			1: {MarketIndex: 1, MarketType: perptypes.MarketTypePerps, Status: perptypes.MarketStatusActive},
		},
		details: map[uint32]markettypes.MarketDetails{
			1: {MarketIndex: 1, FundingRatePrefixSum: math.ZeroInt(), AggregatePremiumSum: math.ZeroInt()},
		},
	}
	ok := stubOracle{price: oracletypes.OraclePrice{IndexPrice: 49_500, MarkPrice: 50_000}}
	bk := stubBook{bid: 49_999, ask: 50_001}

	k, ctx := newFundingKeeper(t, mk, ok, bk)
	require.NoError(t, k.BeginBlocker(ctx))
	got := mk.details[1]
	expected := int64(49_999-49_500) * perptypes.FundingRateTick / int64(49_500)
	require.EqualValues(t, expected, got.AggregatePremiumSum.Int64())
	require.EqualValues(t, 1, got.TotalPremiumSamples)
	require.EqualValues(t, 49_999, got.ImpactBidPrice)
	require.EqualValues(t, 50_001, got.ImpactAskPrice)
}

// TestProcessMarketSample_PerMinuteThrottle verifies the per-market 1-minute
// sampling cadence: a second BeginBlocker within the same minute must skip
// the sample, while a third one ~70s later must admit it.
func TestProcessMarketSample_PerMinuteThrottle(t *testing.T) {
	mk := &stubMarket{
		markets: map[uint32]markettypes.Market{
			1: {MarketIndex: 1, MarketType: perptypes.MarketTypePerps, Status: perptypes.MarketStatusActive},
		},
		details: map[uint32]markettypes.MarketDetails{
			1: {MarketIndex: 1, FundingRatePrefixSum: math.ZeroInt(), AggregatePremiumSum: math.ZeroInt()},
		},
	}
	ok := stubOracle{price: oracletypes.OraclePrice{IndexPrice: 49_500, MarkPrice: 50_000}}
	bk := stubBook{bid: 49_999, ask: 50_001}

	k, ctx := newFundingKeeper(t, mk, ok, bk)

	// First sample admitted (LastPremiumSampleTimestamp == 0 on details).
	require.NoError(t, k.BeginBlocker(ctx))
	require.EqualValues(t, 1, mk.details[1].TotalPremiumSamples)
	premiumAfter1 := mk.details[1].AggregatePremiumSum

	// Second sample 30s later -- throttled by per-market 1-minute window.
	ctx30 := ctx.WithBlockTime(ctx.BlockTime().Add(30 * time.Second))
	require.NoError(t, k.Metadata.Set(ctx30, fundingtypes.FundingMetadata{
		LastFundingRoundTimestamp: ctx30.BlockTime().UnixMilli(),
	}))
	require.NoError(t, k.BeginBlocker(ctx30))
	require.EqualValues(t, 1, mk.details[1].TotalPremiumSamples, "second sample within 1m must be throttled")
	require.True(t, premiumAfter1.Equal(mk.details[1].AggregatePremiumSum))

	// Third sample 70s later -- admitted.
	ctx70 := ctx.WithBlockTime(ctx.BlockTime().Add(70 * time.Second))
	require.NoError(t, k.Metadata.Set(ctx70, fundingtypes.FundingMetadata{
		LastFundingRoundTimestamp: ctx70.BlockTime().UnixMilli(),
	}))
	require.NoError(t, k.BeginBlocker(ctx70))
	require.EqualValues(t, 2, mk.details[1].TotalPremiumSamples, "sample 70s after the previous one must be admitted")
}

// TestProcessMarketSample_MaxPremiumSampleCount stops accumulating once the
// configured cap is reached. The 1-minute throttle is bypassed by setting
// LastPremiumSampleTimestamp far enough in the past.
func TestProcessMarketSample_MaxPremiumSampleCount(t *testing.T) {
	mk := &stubMarket{
		markets: map[uint32]markettypes.Market{
			1: {MarketIndex: 1, MarketType: perptypes.MarketTypePerps, Status: perptypes.MarketStatusActive},
		},
		details: map[uint32]markettypes.MarketDetails{
			1: {
				MarketIndex:          1,
				FundingRatePrefixSum: math.ZeroInt(),
				TotalPremiumSamples:  50,
				AggregatePremiumSum:  math.NewInt(777),
			},
		},
	}
	ok := stubOracle{price: oracletypes.OraclePrice{IndexPrice: 49_500, MarkPrice: 50_000}}
	bk := stubBook{bid: 49_999, ask: 50_001}

	k, ctx := newFundingKeeper(t, mk, ok, bk)
	mk.details[1] = func() markettypes.MarketDetails {
		d := mk.details[1]
		d.LastPremiumSampleTimestamp = ctx.BlockTime().UnixMilli() - 2*perptypes.MinuteInMs
		return d
	}()

	params, err := k.Params.Get(ctx)
	require.NoError(t, err)
	params.MaxPremiumSampleCount = 50
	require.NoError(t, k.Params.Set(ctx, params))
	require.NoError(t, k.Metadata.Set(ctx, fundingtypes.FundingMetadata{
		LastFundingRoundTimestamp: ctx.BlockTime().UnixMilli(),
	}))

	require.NoError(t, k.BeginBlocker(ctx))
	require.EqualValues(t, 50, mk.details[1].TotalPremiumSamples)
	require.EqualValues(t, 777, mk.details[1].AggregatePremiumSum.Int64())
}

// TestSettleMarket_Formula pins down the clamp/divisor logic and the
// mark*rate prefix-sum convention.
//
// BeginBlocker now runs `refreshMarkPrice` BEFORE settling, recomputing
// `MarkPrice` as median(impact_price, index + ema(clamp(impact-idx,
// ±idx/200)), oracle_mark). In this fixture the orderbook stub reports
// both sides ok=false so d.ImpactPrice stays at 0; impact=0 freezes
// premium_raw=0 and the medianer falls back to mean(price_1, price_2):
//
//	premium_raw = 0  (impact=0)
//	premium_ema = 0  (first call reseeds to raw)
//	price_1 = clampUint32(49500 + 0) = 49500
//	price_2 = oracle_mark = 50000
//	mark    = mean(price_1, price_2) = 49750  (impact=0 ⇒ mean fallback)
//
// Settle then runs with:
//
//	premium=10101, ir=0, SmallClamp=500, BigClamp=40000, divisor=8, mark=49750
//	correction = clamp(0 - 10101, -500, +500) = -500
//	smallClamped = 10101 + (-500) = 9601
//	bigClamped = clamp(9601, -40000, +40000) = 9601
//	rate = 9601 / 8 = 1200 (truncated)
//	prefix increment = mark * rate = 49_750 * 1200 = 59_700_000
func TestSettleMarket_Formula(t *testing.T) {
	mk := &stubMarket{
		markets: map[uint32]markettypes.Market{
			1: {MarketIndex: 1, MarketType: perptypes.MarketTypePerps, Status: perptypes.MarketStatusActive},
		},
		details: map[uint32]markettypes.MarketDetails{
			1: {
				MarketIndex:          1,
				MarkPrice:            50_000,
				IndexPrice:           49_500,
				FundingRatePrefixSum: math.ZeroInt(),
				AggregatePremiumSum:  math.NewInt(10_101 * 60), // avg = 10101
				TotalPremiumSamples:  60,
				FundingClampSmall:    500,
				FundingClampBig:      40_000,
				InterestRate:         0,
			},
		},
	}
	ok := stubOracle{price: oracletypes.OraclePrice{IndexPrice: 49_500, MarkPrice: 50_000}}
	// Make ComputeImpactPrice return false so the begin-blocker pre-settle
	// sample step does not mutate the aggregate we just primed.
	bk := stubBook{}

	k, ctx := newFundingKeeper(t, mk, ok, bk)
	// Force the settle branch by stepping past `FundingPeriodMs`.
	require.NoError(t, k.Metadata.Set(ctx, fundingtypes.FundingMetadata{
		LastFundingRoundTimestamp: ctx.BlockTime().UnixMilli() - perptypes.FundingPeriod - 1,
	}))
	require.NoError(t, k.BeginBlocker(ctx))

	got := mk.details[1]
	// Pin the refreshMarkPrice path: refreshMarkPrice must have
	// rewritten MarkPrice 50_000 → 49_750 BEFORE settleMarket
	// consumed it. If a future refactor accidentally moves the
	// refresh after settle (or skips it entirely), this assertion
	// catches the regression before the prefix-sum check fires below.
	require.EqualValues(t, 49_750, got.MarkPrice,
		"refreshMarkPrice must have re-derived MarkPrice from the median pipeline before settle")
	require.EqualValues(t, 59_700_000, got.FundingRatePrefixSum.Int64(),
		"prefix sum must grow by mark * rate (with rate = clamped/divisor)")
	require.True(t, got.AggregatePremiumSum.IsZero(), "settle must reset aggregate")
	require.EqualValues(t, 0, got.TotalPremiumSamples, "settle must reset sample count")
}

// TestSettleMarket_NoSamples leaves the prefix sum untouched when the
// settlement window collected no samples (all blocks fell within the
// throttle, or the market just came online).
func TestSettleMarket_NoSamples(t *testing.T) {
	mk := &stubMarket{
		markets: map[uint32]markettypes.Market{
			1: {MarketIndex: 1, MarketType: perptypes.MarketTypePerps, Status: perptypes.MarketStatusActive},
		},
		details: map[uint32]markettypes.MarketDetails{
			1: {
				MarketIndex:          1,
				FundingRatePrefixSum: math.NewInt(123),
				AggregatePremiumSum:  math.ZeroInt(),
				TotalPremiumSamples:  0,
			},
		},
	}
	ok := stubOracle{price: oracletypes.OraclePrice{IndexPrice: 49_500, MarkPrice: 50_000}}
	bk := stubBook{}

	k, ctx := newFundingKeeper(t, mk, ok, bk)
	// Force the settle branch by stepping past `FundingPeriodMs`.
	require.NoError(t, k.Metadata.Set(ctx, fundingtypes.FundingMetadata{
		LastFundingRoundTimestamp: ctx.BlockTime().UnixMilli() - perptypes.FundingPeriod - 1,
	}))
	require.NoError(t, k.BeginBlocker(ctx))
	require.EqualValues(t, 123, mk.details[1].FundingRatePrefixSum.Int64())
}

// spyOracle counts every GetPrice invocation per market and optionally
// records callers that should never reach it. Used by
// `TestSettleMarket_NoOracleCallInSettlePath` to assert the new contract:
// settle reads `d.MarkPrice` from MarketDetails and never queries the oracle
// directly.
type spyOracle struct {
	calls map[uint32]int
}

func (s *spyOracle) GetPrice(_ context.Context, marketIdx uint32) (oracletypes.OraclePrice, error) {
	if s.calls == nil {
		s.calls = map[uint32]int{}
	}
	s.calls[marketIdx]++
	return oracletypes.OraclePrice{}, oracletypes.ErrStalePrice.Wrap("spy never returns success")
}

// TestSettleMarket_NoOracleCallInSettlePath pins down the structural
// invariant that `settleMarket` must not call the oracle. We:
//
//  1. Pre-prime the market with TotalPremiumSamples=60 + cached MarkPrice so
//     the settle path has everything it needs.
//  2. Set `d.LastPremiumSampleTimestamp = now` so the per-minute throttle drops
//     `processMarketSample` before it can touch the oracle.
//  3. Step past `FundingPeriodMs` so the settle branch fires.
//  4. Run BeginBlocker with a spying oracle that always errors out.
//
// Note: `refreshMarkPrice` (which runs every block before settle) DOES
// call the oracle. So the spy will see exactly one call — from
// refreshMarkPrice — and zero calls from settleMarket. Because the spy
// returns an error, refreshMarkPrice bails out early without mutating
// d.MarkPrice, so the cached `50_000` survives into settleMarket and
// the prefix increment matches the expected value.
func TestSettleMarket_NoOracleCallInSettlePath(t *testing.T) {
	mk := &stubMarket{
		markets: map[uint32]markettypes.Market{
			1: {MarketIndex: 1, MarketType: perptypes.MarketTypePerps, Status: perptypes.MarketStatusActive},
		},
		details: map[uint32]markettypes.MarketDetails{
			1: {
				MarketIndex:          1,
				MarkPrice:            50_000, // cached from a previous sample
				IndexPrice:           49_500,
				FundingRatePrefixSum: math.ZeroInt(),
				AggregatePremiumSum:  math.NewInt(10_101 * 60),
				TotalPremiumSamples:  60,
				FundingClampSmall:    500,
				FundingClampBig:      40_000,
			},
		},
	}
	spy := &spyOracle{}
	bk := stubBook{}

	k, ctx := newFundingKeeper(t, mk, spy, bk)
	mk.details[1] = func() markettypes.MarketDetails {
		d := mk.details[1]
		// Pin LastPremiumSampleTimestamp to now so the 1-minute throttle drops
		// processMarketSample before it reaches GetPrice.
		d.LastPremiumSampleTimestamp = ctx.BlockTime().UnixMilli()
		return d
	}()
	require.NoError(t, k.Metadata.Set(ctx, fundingtypes.FundingMetadata{
		LastFundingRoundTimestamp: ctx.BlockTime().UnixMilli() - perptypes.FundingPeriod - 1,
	}))

	require.NoError(t, k.BeginBlocker(ctx))
	// Exactly one oracle call: refreshMarkPrice. processMarketSample
	// was throttled out and settleMarket reads only the cached
	// MarkPrice -- no oracle call from the settle path itself.
	require.Equal(t, 1, spy.calls[1],
		"only refreshMarkPrice may query oracle (1 call); settleMarket itself must read only cached state")

	got := mk.details[1]
	require.EqualValues(t, 60_000_000, got.FundingRatePrefixSum.Int64())
	require.True(t, got.AggregatePremiumSum.IsZero())
	require.EqualValues(t, 0, got.TotalPremiumSamples)
}

// TestProcessMarketSample_LargePriceDoesNotOverflow feeds an impact
// price near uint32-max so the `(IB - idx) * FundingRateTick`
// intermediate (~ 4.29e9 * 1e6 = 4.29e15) plus accumulation across 60
// samples stays inside math.Int. The test asserts the post-sample
// state is sane (no panic, premium in the expected ballpark).
func TestProcessMarketSample_LargePriceDoesNotOverflow(t *testing.T) {
	const ib = uint32(4_000_000_000)
	const ia = uint32(4_000_000_002)
	const idx = uint32(3_999_999_500)
	mk := &stubMarket{
		markets: map[uint32]markettypes.Market{
			1: {MarketIndex: 1, MarketType: perptypes.MarketTypePerps, Status: perptypes.MarketStatusActive},
		},
		details: map[uint32]markettypes.MarketDetails{
			1: {
				MarketIndex:          1,
				FundingRatePrefixSum: math.ZeroInt(),
				AggregatePremiumSum:  math.ZeroInt(),
			},
		},
	}
	ok := stubOracle{price: oracletypes.OraclePrice{IndexPrice: idx, MarkPrice: idx}}
	bk := stubBook{bid: ib, ask: ia}

	k, ctx := newFundingKeeper(t, mk, ok, bk)
	require.NoError(t, k.BeginBlocker(ctx))

	// Expected premium: (ib - idx) * TICK / idx = 500 * 1e6 / 3_999_999_500 = 0
	// (truncates to zero -- still correct, no overflow).
	got := mk.details[1]
	require.EqualValues(t, 1, got.TotalPremiumSamples)
	// The math.Int implementation handles the multiplication without
	// overflowing int64; the assertion below merely confirms the value is
	// non-negative and bounded.
	require.True(t, got.AggregatePremiumSum.GTE(math.ZeroInt()))
	require.True(t, got.AggregatePremiumSum.LTE(math.NewInt(perptypes.FundingRateTick)))
}
