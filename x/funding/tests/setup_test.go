// Package tests hosts the external (`package tests`) test suite for the
// `x/funding` module. The files in this directory are split by business
// domain rather than mirroring the production source layout:
//
//   - setup_test.go             — shared fixtures, stubs and keeper builders
//   - process_sample_test.go    — per-block premium sampling (processMarketSample)
//   - settle_market_test.go     — per-round market settlement (settleMarket / SettleAllMarkets)
//   - settle_position_test.go   — per-position funding settlement (SettlePositionFunding)
//   - refresh_mark_price_test.go — per-block mark-price refresh pipeline
//   - query_test.go             — gRPC query handlers
//
// All helpers in this file are package-private and exclusively used by
// the sibling `*_test.go` files.
package tests

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

// stubMarket implements the funding module's MarketKeeper surface with an
// in-memory map. `sets` counts the number of SetMarketDetails calls so
// tests can assert write-back behaviour.
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

// stubOracle returns a fixed price (or error) by default and supports
// per-market overrides via the `prices` / `errs` maps. Both maps may
// be nil; tests that only care about a single market use the bare
// `price` / `err` fields.
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

// stubAccount is a stateless AccountKeeper stub: every GetPosition
// returns a fresh zero-valued AccountPosition. The funding keeper's
// SettlePositionFunding short-circuits on BaseSize == 0 so this stub
// is sufficient for the prefix-sum / mark-price tests that don't care
// about persisted writes; use `statefulAccount` for tests that need
// to observe ApplyFundingPayment output.
type stubAccount struct{}

func (stubAccount) GetPosition(_ context.Context, acc uint64, mkt uint32) (accounttypes.AccountPosition, error) {
	return accounttypes.AccountPosition{
		AccountIndex: acc, MarketIndex: mkt,
		BaseSize: math.ZeroInt(), EntryQuote: math.ZeroInt(),
		LastFundingRatePrefixSum: math.ZeroInt(), AllocatedMargin: math.ZeroInt(),
	}, nil
}

// ApplyFundingPayment is the cohesive funding-settlement RMW on the
// real keeper. The stateless stub returns BaseSize == 0 from
// GetPosition, so this is exercised only for interface parity — the
// production short-circuit in ApplyFundingPayment matches the same
// behaviour (no-op on empty rows).
func (s stubAccount) ApplyFundingPayment(
	ctx context.Context,
	accIdx uint64,
	marketIdx uint32,
	_ math.Int,
) (accounttypes.AccountPosition, error) {
	return s.GetPosition(ctx, accIdx, marketIdx)
}

// statefulAccount is a map-backed AccountKeeper stub used by tests
// that need to read back the position state mutated by
// SettlePositionFunding.
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

// ApplyFundingPayment mirrors the cohesive funding-settlement RMW so
// SettlePositionFunding hits this stub. Folds the payment into
// EntryQuote and snapshots the prefix sum, matching the production
// math (`pay = BaseSize * (newPrefix - lastPrefix) / FundingRateTick`).
func (s *statefulAccount) ApplyFundingPayment(
	ctx context.Context,
	accIdx uint64,
	marketIdx uint32,
	newPrefixSum math.Int,
) (accounttypes.AccountPosition, error) {
	pos, err := s.GetPosition(ctx, accIdx, marketIdx)
	if err != nil {
		return accounttypes.AccountPosition{}, err
	}
	if pos.BaseSize.IsZero() || newPrefixSum.IsNil() {
		return pos, nil
	}
	delta := newPrefixSum.Sub(pos.LastFundingRatePrefixSum)
	if delta.IsZero() {
		return pos, nil
	}
	pay := pos.BaseSize.Mul(delta).Quo(math.NewInt(perptypes.FundingRateTick))
	pos.EntryQuote = pos.EntryQuote.Add(pay)
	pos.LastFundingRatePrefixSum = newPrefixSum
	if err := s.SetPosition(ctx, pos); err != nil {
		return accounttypes.AccountPosition{}, err
	}
	return pos, nil
}

// spyOracle counts every GetPrice invocation per market. Used by
// tests that need to assert how many times the settle / refresh
// pipeline reads from the oracle.
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

// newFundingKeeper boots a funding keeper backed by in-memory stores and
// the supplied stubs. The keeper's `Metadata.LastFundingRoundTimestamp`
// is initialised to BlockTime so the begin-blocker's
// `LastFundingRoundTimestamp == 0` short-circuit does not fire — tests
// can then drive `processMarketSample` in isolation, and the few tests
// that need the settle path step the timestamp past `FundingPeriodMs`
// explicitly.
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
	require.NoError(t, k.Metadata.Set(ctx, fundingtypes.FundingMetadata{
		LastFundingRoundTimestamp: ctx.BlockTime().UnixMilli(),
	}))
	return k, ctx
}

// newFundingKeeperWithAccount is `newFundingKeeper` plus a stateful
// account stub so tests can observe position-level funding writes.
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
