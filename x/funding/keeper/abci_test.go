package keeper_test

import (
	"context"
	"errors"
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

type stubBook struct {
	bid, ask uint32
}

func (s stubBook) BestBidAsk(_ context.Context, _ uint32) (uint32, uint32, error) {
	return s.bid, s.ask, nil
}
func (s stubBook) ComputeImpactPrice(_ context.Context, _ uint32, _ bool, _ uint64) (uint32, bool, error) {
	return 0, false, nil
}

type stubOracle struct{ price oracletypes.OraclePrice }

func (s stubOracle) GetPrice(_ context.Context, _ uint32) (oracletypes.OraclePrice, error) {
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

func newFundingKeeper(t *testing.T, mk *stubMarket, ok stubOracle, bk stubBook) (fundingkeeper.Keeper, sdk.Context) {
	t.Helper()
	keys := storetypes.NewKVStoreKeys(fundingtypes.StoreKey)
	cdc := moduletestutil.MakeTestEncodingConfig().Codec
	cms := integration.CreateMultiStore(keys, log.NewTestLogger(t))
	ctx := sdk.NewContext(cms, cmtprototypes.Header{}, true, log.NewTestLogger(t))
	k := fundingkeeper.NewKeeper(
		cdc,
		runtime.NewKVStoreService(keys[fundingtypes.StoreKey]),
		"auth",
		mk, ok, bk, stubAccount{},
	)
	require.NoError(t, k.Params.Set(ctx, fundingtypes.DefaultParams()))
	require.NoError(t, k.Metadata.Set(ctx, fundingtypes.FundingMetadata{}))
	return k, ctx
}

// TestProcessMarketSample_OneSidedClearsImpactPrice verifies that when only
// one side of the book has depth we clear `ImpactPrice` instead of leaking
// a stale cross price into the premium accumulator (audit Medium funding-14).
func TestProcessMarketSample_OneSidedClearsImpactPrice(t *testing.T) {
	mk := &stubMarket{
		markets: map[uint32]markettypes.Market{
			1: {MarketIndex: 1, MarketType: perptypes.MarketTypePerps, Status: perptypes.MarketStatusActive},
		},
		details: map[uint32]markettypes.MarketDetails{
			1: {
				MarketIndex:          1,
				ImpactPrice:          999, // stale
				FundingRatePrefixSum: math.ZeroInt(),
			},
		},
	}
	ok := stubOracle{price: oracletypes.OraclePrice{IndexPrice: 100, MarkPrice: 100}}
	bk := stubBook{bid: 0, ask: 110} // one-sided book

	k, ctx := newFundingKeeper(t, mk, ok, bk)
	require.NoError(t, k.BeginBlocker(ctx))
	require.EqualValues(t, 0, mk.details[1].ImpactPrice)
}

// TestProcessMarketSample_MaxPremiumSampleCount stops accumulating once the
// configured cap is reached.
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
	ok := stubOracle{price: oracletypes.OraclePrice{IndexPrice: 100, MarkPrice: 100}}
	bk := stubBook{bid: 99, ask: 101}

	k, ctx := newFundingKeeper(t, mk, ok, bk)
	// Force a small cap so the next sample is skipped.
	params, err := k.Params.Get(ctx)
	require.NoError(t, err)
	params.MaxPremiumSampleCount = 50
	require.NoError(t, k.Params.Set(ctx, params))
	// Pretend funding was just settled so BeginBlocker does not re-settle
	// and reset the counters this pass.
	require.NoError(t, k.Metadata.Set(ctx, fundingtypes.FundingMetadata{
		LastFundingRoundTimestamp: ctx.BlockTime().UnixMilli(),
	}))

	require.NoError(t, k.BeginBlocker(ctx))
	require.EqualValues(t, 50, mk.details[1].TotalPremiumSamples)
	require.EqualValues(t, 777, mk.details[1].AggregatePremiumSum)
}
