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
	return markettypes.MarketDetails{MarketIndex: idx, FundingRatePrefixSum: math.ZeroInt()}, nil
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

// stubBook fakes the orderbook for funding sampler tests. We expose separate
// "ok" flags per side so a test can simulate one-sided depth.
type stubBook struct {
	bid, ask     uint32
	bidOk, askOk bool
}

func (s stubBook) BestBidAsk(_ context.Context, _ uint32) (uint32, uint32, error) {
	return s.bid, s.ask, nil
}
func (s stubBook) ComputeImpactPrice(_ context.Context, _ uint32, isAsk bool, _ uint64) (uint32, bool, error) {
	if isAsk {
		return s.ask, s.askOk, nil
	}
	return s.bid, s.bidOk, nil
}
func (stubBook) ImpactUsdcAmount(_ context.Context) (uint64, error) {
	return perptypes.ImpactUSDCAmount, nil
}

type stubOracle struct {
	price oracletypes.OraclePrice
	err   error
}

func (s stubOracle) GetPrice(_ context.Context, _ uint32) (oracletypes.OraclePrice, error) {
	if s.err != nil {
		return oracletypes.OraclePrice{}, s.err
	}
	return s.price, nil
}

type stubAccount struct{}

func (stubAccount) GetPosition(_ context.Context, acc uint64, mkt uint32) (accounttypes.AccountPosition, error) {
	return accounttypes.AccountPosition{
		AccountIndex: acc, MarketIndex: mkt,
		Position: math.ZeroInt(), EntryQuote: math.ZeroInt(),
		LastFundingRatePrefixSum: math.ZeroInt(), AllocatedMargin: math.ZeroInt(),
	}, nil
}
func (stubAccount) SetPosition(_ context.Context, _ accounttypes.AccountPosition) error { return nil }

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
		Position: math.ZeroInt(), EntryQuote: math.ZeroInt(),
		LastFundingRatePrefixSum: math.ZeroInt(), AllocatedMargin: math.ZeroInt(),
	}, nil
}

func (s *statefulAccount) SetPosition(_ context.Context, p accounttypes.AccountPosition) error {
	key := [2]uint64{p.AccountIndex, uint64(p.MarketIndex)}
	s.positions[key] = p
	return nil
}

func newFundingKeeper(t *testing.T, mk *stubMarket, ok stubOracle, bk stubBook) (fundingkeeper.Keeper, sdk.Context) {
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

// TestSettlePositionFunding_ZeroPositionSnapshotsCurrentPrefix ensures a fresh
// or fully closed position starts from the current funding prefix instead of
// inheriting all historical funding accumulated before it opened.
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
			},
		},
	}
	ak := newStatefulAccount()
	k, ctx := newFundingKeeperWithAccount(
		t,
		mk,
		stubOracle{price: oracletypes.OraclePrice{IndexPrice: 49_500, MarkPrice: 50_000}},
		stubBook{bidOk: true, askOk: true},
		ak,
	)

	require.NoError(t, k.SettlePositionFunding(ctx, accountIndex, marketIndex))
	key := [2]uint64{accountIndex, uint64(marketIndex)}
	snapshotted := ak.positions[key]
	require.True(t, snapshotted.Position.IsZero())
	require.EqualValues(t, 100_000_000, snapshotted.LastFundingRatePrefixSum.Int64())

	// Simulate ApplyPerpsMatching opening a new position after the zero-size
	// settle above, then advance the market prefix by only 20_000_000. The next
	// funding settlement must charge that new delta, not the full historical
	// 120_000_000 prefix.
	snapshotted.Position = math.NewInt(1_000_000)
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

func TestProcessMarketSample_StaleOracleReturnsError(t *testing.T) {
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
			},
		},
	}
	k, ctx := newFundingKeeper(
		t,
		mk,
		stubOracle{err: oracletypes.ErrStalePrice.Wrap("stale fixture")},
		stubBook{bid: 49_999, ask: 50_001, bidOk: true, askOk: true},
	)

	require.NoError(t, k.BeginBlocker(ctx))
	got := mk.details[1]
	require.EqualValues(t, 0, got.TotalPremiumSamples)
	require.EqualValues(t, 0, got.AggregatePremiumSum)
	require.EqualValues(t, 0, got.LastUpdatedTimestamp, "stale oracle must not throttle retries")
}

func TestSettleMarket_StaleOracleDoesNotAdvancePrefixOrRound(t *testing.T) {
	oldRoundTs := int64(1_699_996_399_999)
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
				AggregatePremiumSum:  10_101 * 60,
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
		stubBook{bidOk: false, askOk: false},
	)
	require.NoError(t, k.Metadata.Set(ctx, fundingtypes.FundingMetadata{
		LastFundingRoundTimestamp: oldRoundTs,
	}))

	require.NoError(t, k.BeginBlocker(ctx))
	got := mk.details[1]
	require.True(t, got.FundingRatePrefixSum.IsZero())
	require.EqualValues(t, 10_101*60, got.AggregatePremiumSum)
	require.EqualValues(t, 60, got.TotalPremiumSamples)
	meta, metaErr := k.Metadata.Get(ctx)
	require.NoError(t, metaErr)
	require.EqualValues(t, oldRoundTs, meta.LastFundingRoundTimestamp)
}

func TestMarketFundingRateQuery_ReturnsFundingRoundTimestamp(t *testing.T) {
	mk := &stubMarket{
		markets: map[uint32]markettypes.Market{
			1: {MarketIndex: 1, MarketType: perptypes.MarketTypePerps, Status: perptypes.MarketStatusActive},
		},
		details: map[uint32]markettypes.MarketDetails{
			1: {
				MarketIndex:          1,
				FundingRatePrefixSum: math.NewInt(123),
				LastUpdatedTimestamp: 456,
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
				AggregatePremiumSum:  42,  // baseline that must not move
				TotalPremiumSamples:  1,
				FundingRatePrefixSum: math.ZeroInt(),
			},
		},
	}
	ok := stubOracle{price: oracletypes.OraclePrice{IndexPrice: 100, MarkPrice: 100}}
	bk := stubBook{bid: 0, ask: 110, bidOk: false, askOk: true} // ask depth only

	k, ctx := newFundingKeeper(t, mk, ok, bk)
	require.NoError(t, k.BeginBlocker(ctx))
	got := mk.details[1]
	require.EqualValues(t, 0, got.ImpactPrice, "stale impact mid must be cleared")
	require.EqualValues(t, 0, got.ImpactBidPrice)
	require.EqualValues(t, 110, got.ImpactAskPrice)
	require.EqualValues(t, 42, got.AggregatePremiumSum, "premium sum must not move when a side has no depth")
	require.EqualValues(t, 1, got.TotalPremiumSamples, "sample count must not advance")
}

// TestProcessMarketSample_LighterPremium drives the new formula directly:
//
//	premium_t = ( max(0, IB - idx) - max(0, idx - IA) ) * TICK / idx
//
// With IB=49999, IA=50001 and idx=49500 we expect
// `(49999-49500) * 1e6 / 49500 = 499*1e6/49500 = 10080`.
func TestProcessMarketSample_LighterPremium(t *testing.T) {
	mk := &stubMarket{
		markets: map[uint32]markettypes.Market{
			1: {MarketIndex: 1, MarketType: perptypes.MarketTypePerps, Status: perptypes.MarketStatusActive},
		},
		details: map[uint32]markettypes.MarketDetails{
			1: {MarketIndex: 1, FundingRatePrefixSum: math.ZeroInt()},
		},
	}
	ok := stubOracle{price: oracletypes.OraclePrice{IndexPrice: 49_500, MarkPrice: 50_000}}
	bk := stubBook{bid: 49_999, ask: 50_001, bidOk: true, askOk: true}

	k, ctx := newFundingKeeper(t, mk, ok, bk)
	require.NoError(t, k.BeginBlocker(ctx))
	got := mk.details[1]
	expected := int64(49_999-49_500) * perptypes.FundingRateTick / int64(49_500)
	require.EqualValues(t, expected, got.AggregatePremiumSum)
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
			1: {MarketIndex: 1, FundingRatePrefixSum: math.ZeroInt()},
		},
	}
	ok := stubOracle{price: oracletypes.OraclePrice{IndexPrice: 49_500, MarkPrice: 50_000}}
	bk := stubBook{bid: 49_999, ask: 50_001, bidOk: true, askOk: true}

	k, ctx := newFundingKeeper(t, mk, ok, bk)

	// First sample admitted (LastUpdatedTimestamp == 0 on details).
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
	require.EqualValues(t, premiumAfter1, mk.details[1].AggregatePremiumSum)

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
// LastUpdatedTimestamp far enough in the past.
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
				AggregatePremiumSum:  777,
			},
		},
	}
	ok := stubOracle{price: oracletypes.OraclePrice{IndexPrice: 49_500, MarkPrice: 50_000}}
	bk := stubBook{bid: 49_999, ask: 50_001, bidOk: true, askOk: true}

	k, ctx := newFundingKeeper(t, mk, ok, bk)
	mk.details[1] = func() markettypes.MarketDetails {
		d := mk.details[1]
		d.LastUpdatedTimestamp = ctx.BlockTime().UnixMilli() - 2*perptypes.MinuteInMs
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
	require.EqualValues(t, 777, mk.details[1].AggregatePremiumSum)
}

// TestSettleMarket_LighterFormula pins down the new clamp/divisor logic and
// the mark*rate prefix-sum convention.
//
//	premium=10101, ir=0, SmallClamp=500, BigClamp=40000, divisor=8, mark=50000
//	correction = clamp(0 - 10101, -500, +500) = -500
//	smallClamped = 10101 + (-500) = 9601
//	bigClamped = clamp(9601, -40000, +40000) = 9601
//	rate = 9601 / 8 = 1200 (truncated)
//	prefix increment = mark * rate = 50_000 * 1200 = 60_000_000
func TestSettleMarket_LighterFormula(t *testing.T) {
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
				AggregatePremiumSum:  10_101 * 60, // avg = 10101
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
	bk := stubBook{bid: 0, ask: 0, bidOk: false, askOk: false}

	k, ctx := newFundingKeeper(t, mk, ok, bk)
	// Force the settle branch by stepping past `FundingPeriodMs`.
	require.NoError(t, k.Metadata.Set(ctx, fundingtypes.FundingMetadata{
		LastFundingRoundTimestamp: ctx.BlockTime().UnixMilli() - perptypes.FundingPeriod - 1,
	}))
	require.NoError(t, k.BeginBlocker(ctx))

	got := mk.details[1]
	require.EqualValues(t, 60_000_000, got.FundingRatePrefixSum.Int64(),
		"prefix sum must grow by mark * rate (with rate = clamped/divisor)")
	require.EqualValues(t, 0, got.AggregatePremiumSum, "settle must reset aggregate")
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
				TotalPremiumSamples:  0,
			},
		},
	}
	ok := stubOracle{price: oracletypes.OraclePrice{IndexPrice: 49_500, MarkPrice: 50_000}}
	bk := stubBook{bidOk: false, askOk: false}

	k, ctx := newFundingKeeper(t, mk, ok, bk)
	// Force the settle branch by stepping past `FundingPeriodMs`.
	require.NoError(t, k.Metadata.Set(ctx, fundingtypes.FundingMetadata{
		LastFundingRoundTimestamp: ctx.BlockTime().UnixMilli() - perptypes.FundingPeriod - 1,
	}))
	require.NoError(t, k.BeginBlocker(ctx))
	require.EqualValues(t, 123, mk.details[1].FundingRatePrefixSum.Int64())
}
