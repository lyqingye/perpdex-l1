// setup_test.go owns the in-memory fixture that every matching test
// composes against. The fixture stands up:
//
//   - a real `orderbook/keeper.Keeper` over an in-memory store, so book
//     mechanics (open/cancel/iterate/evict, account-open + client-id
//     indexes) execute with production semantics;
//   - a real `matching/keeper.Keeper` wired through `NewKeeper`, with
//     `Params` seeded so cap-bounded loops behave deterministically;
//   - lightweight stubs for the dependencies that are not under test:
//     account / market / trade. The stubs only model the slice of
//     behaviour each test actually exercises (positions, market
//     metadata, recorded fills, nonce allocation).
//
// The stubs intentionally stay package-private — they are NOT exported
// from `x/matching/keeper`. Test packages outside this directory must
// re-implement their own doubles rather than couple to these structs.
package tests

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
	matchingkeeper "github.com/perpdex/perpdex-l1/x/matching/keeper"
	matchingtypes "github.com/perpdex/perpdex-l1/x/matching/types"
	orderbookkeeper "github.com/perpdex/perpdex-l1/x/orderbook/keeper"
	orderbooktypes "github.com/perpdex/perpdex-l1/x/orderbook/types"
	tradekeeper "github.com/perpdex/perpdex-l1/x/trade/keeper"
)

// --- account stub ---------------------------------------------------------

// stubAccount models the slice of x/account the matching loop reads:
// IsAuthorized (always true here — authority is asserted by msg-server
// tests directly), GetAccount (returns a master account so the pool
// gate in CreateOrder lets the test order through), GetPosition (the
// per-(account, market) position the reduce-only and liquidation paths
// consult), and AvailableBalance (infinite for matching tests so the
// spot pre-rest gate never trips; dedicated lock tests live in
// x/orderbook).
type stubAccount struct {
	pos map[[2]uint64]accounttypes.AccountPosition
}

func newStubAccount() *stubAccount {
	return &stubAccount{pos: map[[2]uint64]accounttypes.AccountPosition{}}
}

func key(acc uint64, mkt uint32) [2]uint64 { return [2]uint64{acc, uint64(mkt)} }

func (s *stubAccount) GetAccount(_ context.Context, idx uint64) (accounttypes.Account, error) {
	return accounttypes.Account{AccountIndex: idx, AccountType: perptypes.MasterAccountType, Collateral: math.ZeroInt()}, nil
}

func (s *stubAccount) GetPosition(_ context.Context, acc uint64, mkt uint32) (accounttypes.AccountPosition, error) {
	if p, ok := s.pos[key(acc, mkt)]; ok {
		return p, nil
	}
	return accounttypes.AccountPosition{
		AccountIndex:             acc,
		MarketIndex:              mkt,
		BaseSize:                 math.ZeroInt(),
		EntryQuote:               math.ZeroInt(),
		LastFundingRatePrefixSum: math.ZeroInt(),
		AllocatedMargin:          math.ZeroInt(),
		MarginMode:               perptypes.CrossMargin,
	}, nil
}

func (s *stubAccount) setPosition(acc uint64, mkt uint32, size int64) {
	s.pos[key(acc, mkt)] = accounttypes.AccountPosition{
		AccountIndex:             acc,
		MarketIndex:              mkt,
		BaseSize:                 math.NewInt(size),
		EntryQuote:               math.ZeroInt(),
		LastFundingRatePrefixSum: math.ZeroInt(),
		AllocatedMargin:          math.ZeroInt(),
		MarginMode:               perptypes.CrossMargin,
	}
}

func (s *stubAccount) IsAuthorized(_ context.Context, _ string, _ uint64) (bool, error) {
	return true, nil
}

func (s *stubAccount) AvailableBalance(_ context.Context, _ uint64, _ uint32) (math.Int, error) {
	// Stub assumes infinite spot liquidity for matching tests; the
	// dedicated spot lock tests live in x/orderbook keeper tests.
	return math.NewInt(1 << 62), nil
}

// --- spot locker stub -----------------------------------------------------

// stubLocker is a no-op SpotLocker. Matching's OpenOrder path threads
// through here for spot residue locking; matching tests don't assert
// lock semantics (those live in x/orderbook) so the stub returns nil.
type stubLocker struct{}

func (stubLocker) IncreaseLockedBalance(_ context.Context, _ uint64, _ uint32, _ math.Int) error {
	return nil
}
func (stubLocker) DecreaseLockedBalance(_ context.Context, _ uint64, _ uint32, _ math.Int) error {
	return nil
}

// --- market stub ----------------------------------------------------------

// stubMarket returns a benign perps market and allocates monotonic
// nonces per side. Tests that need to vary
// `MaxOpenOrdersPerAccount` (e.g. the cap tests) mutate
// `maxOpenOrders` directly before placing orders.
type stubMarket struct {
	maxOpenOrders uint32
	nextNonceAsk  int64
	nextNonceBid  int64
}

func (s *stubMarket) GetMarket(_ context.Context, idx uint32) (markettypes.Market, error) {
	return markettypes.Market{
		MarketIndex:             idx,
		MarketType:              perptypes.MarketTypePerps,
		Status:                  perptypes.MarketStatusActive,
		TakerFee:                0,
		MakerFee:                0,
		MaxOpenOrdersPerAccount: s.maxOpenOrders,
	}, nil
}

func (*stubMarket) GetMarketDetails(_ context.Context, idx uint32) (markettypes.MarketDetails, error) {
	return markettypes.MarketDetails{MarketIndex: idx}, nil
}

func (s *stubMarket) AllocateNonce(_ context.Context, _ uint32, isAsk bool) (int64, error) {
	if isAsk {
		s.nextNonceAsk++
		return s.nextNonceAsk, nil
	}
	s.nextNonceBid--
	return s.nextNonceBid, nil
}

// GetMarkPriceAndDetails returns a benign fresh mark; matching tests that
// exercise the trigger-activation path live in their own files and
// override the keeper directly.
func (*stubMarket) GetMarkPriceAndDetails(_ context.Context, mkt uint32) (uint32, markettypes.MarketDetails, error) {
	return 1, markettypes.MarketDetails{MarketIndex: mkt, MarkPrice: 1, LastMarkPriceRefreshTimestamp: 1}, nil
}

func (*stubMarket) SetMarketDetails(_ context.Context, _ markettypes.MarketDetails) error {
	return nil
}

// --- trade stub -----------------------------------------------------------

// stubFill is a flattened record over the small subset of fill fields
// the matching tests assert against. Captured uniformly across perp
// and spot so existing tests can keep using `.fills` for cardinality
// checks and `.fills[i].BaseAmount` for size checks without juggling
// two slices.
//
// The liquidation-specific fields (ZeroPrice, LiquidationFeeBps,
// LiquidationFeeRecipient, NoRiskCheck, TakerFee, MakerFee, Price)
// are populated from PerpFill so the matching liquidation tests can
// assert end-to-end fill plumbing without standing up the real trade
// keeper.
type stubFill struct {
	MakerAccountIndex       uint64
	TakerAccountIndex       uint64
	MarketIndex             uint32
	Price                   uint32
	BaseAmount              uint64
	IsTakerAsk              bool
	TakerFee                uint32
	MakerFee                uint32
	ZeroPrice               uint32
	LiquidationFeeBps       uint32
	LiquidationFeeRecipient uint64
	NoRiskCheck             bool
	SkipMakerRiskCheck      bool
	SkipTakerRiskCheck      bool
}

// stubTrade records every fill it sees and applies the position delta to
// the linked stubAccount so subsequent matching iterations see the
// updated maker/taker positions (mirroring real trade-keeper behaviour).
type stubTrade struct {
	ak    *stubAccount
	fills []stubFill
}

func (s *stubTrade) applyDelta(acc uint64, mkt uint32, delta int64) {
	cur, _ := s.ak.GetPosition(context.Background(), acc, mkt)
	cur.BaseSize = cur.BaseSize.Add(math.NewInt(delta))
	s.ak.pos[key(acc, mkt)] = cur
}

func (s *stubTrade) ApplyPerpsMatching(_ context.Context, f tradekeeper.PerpFill) error {
	s.fills = append(s.fills, stubFill{
		MakerAccountIndex:       f.MakerAccountIndex,
		TakerAccountIndex:       f.TakerAccountIndex,
		MarketIndex:             f.MarketIndex,
		Price:                   f.Price,
		BaseAmount:              f.BaseAmount,
		IsTakerAsk:              f.IsTakerAsk,
		TakerFee:                f.TakerFee,
		MakerFee:                f.MakerFee,
		ZeroPrice:               f.ZeroPrice,
		LiquidationFeeBps:       f.LiquidationFeeBps,
		LiquidationFeeRecipient: f.LiquidationFeeRecipient,
		NoRiskCheck:             f.NoRiskCheck,
		SkipMakerRiskCheck:      f.SkipMakerRiskCheck,
		SkipTakerRiskCheck:      f.SkipTakerRiskCheck,
	})
	base := int64(f.BaseAmount)
	if f.IsTakerAsk {
		// taker sells, maker buys
		s.applyDelta(f.TakerAccountIndex, f.MarketIndex, -base)
		s.applyDelta(f.MakerAccountIndex, f.MarketIndex, +base)
	} else {
		// taker buys, maker sells
		s.applyDelta(f.TakerAccountIndex, f.MarketIndex, +base)
		s.applyDelta(f.MakerAccountIndex, f.MarketIndex, -base)
	}
	return nil
}

func (s *stubTrade) ApplySpotMatching(_ context.Context, f tradekeeper.SpotFill, _, _ uint32) error {
	s.fills = append(s.fills, stubFill{
		MakerAccountIndex: f.MakerAccountIndex,
		TakerAccountIndex: f.TakerAccountIndex,
		MarketIndex:       f.MarketIndex,
		BaseAmount:        f.BaseAmount,
		IsTakerAsk:        f.IsTakerAsk,
	})
	return nil
}

// --- injecting trade ------------------------------------------------------

// injectingTrade is a TradeKeeper double that returns the next preset
// error from `errs` (consuming one per ApplyPerpsMatching / spot call)
// so the matching loop can be exercised with maker / taker / hard
// failure patterns without standing up the real risk + funding stack.
type injectingTrade struct {
	*stubTrade
	errs []error
}

func (s *injectingTrade) next() error {
	if len(s.errs) == 0 {
		return nil
	}
	err := s.errs[0]
	s.errs = s.errs[1:]
	return err
}

func (s *injectingTrade) ApplyPerpsMatching(ctx context.Context, f tradekeeper.PerpFill) error {
	if err := s.next(); err != nil {
		return err
	}
	return s.stubTrade.ApplyPerpsMatching(ctx, f)
}

func (s *injectingTrade) ApplySpotMatching(ctx context.Context, f tradekeeper.SpotFill, b, q uint32) error {
	if err := s.next(); err != nil {
		return err
	}
	return s.stubTrade.ApplySpotMatching(ctx, f, b, q)
}

// --- risk stubs -----------------------------------------------------------

// stubRisk is a fixed-sequence risk classifier used by the liquidation
// matching tests. `cross` and `iso` return the next status from a
// per-account / per-(account, market) FIFO; an empty slice falls back
// to `defaultStatus`. This lets tests step the victim's health from
// PARTIAL → HEALTHY across loop iterations to exercise the
// `is_not_in_liquidation_and_is_liquidation_order` short-circuit
// without standing up the real risk keeper.
type stubRisk struct {
	defaultStatus uint32
	cross         map[uint64][]uint32
	iso           map[[2]uint64][]uint32
}

func newStubRisk() *stubRisk {
	return &stubRisk{
		defaultStatus: perptypes.HealthHealthy,
		cross:         map[uint64][]uint32{},
		iso:           map[[2]uint64][]uint32{},
	}
}

func (s *stubRisk) GetHealthStatus(_ context.Context, acc uint64) (uint32, error) {
	q := s.cross[acc]
	if len(q) == 0 {
		return s.defaultStatus, nil
	}
	v := q[0]
	s.cross[acc] = q[1:]
	return v, nil
}

func (s *stubRisk) GetIsolatedHealthStatus(_ context.Context, acc uint64, mkt uint32) (uint32, error) {
	k := [2]uint64{acc, uint64(mkt)}
	q := s.iso[k]
	if len(q) == 0 {
		return s.defaultStatus, nil
	}
	v := q[0]
	s.iso[k] = q[1:]
	return v, nil
}

// stubPreLiqRisk is a minimal RiskKeeper used to drive
// CheckPreLiquidationGate: it returns the same cross / iso health
// values on every call. Used by the gate's truth-table tests.
type stubPreLiqRisk struct {
	cross uint32
	iso   uint32
}

func (s stubPreLiqRisk) GetHealthStatus(_ context.Context, _ uint64) (uint32, error) {
	return s.cross, nil
}
func (s stubPreLiqRisk) GetIsolatedHealthStatus(_ context.Context, _ uint64, _ uint32) (uint32, error) {
	return s.iso, nil
}

// --- matching env ---------------------------------------------------------

// matchEnv bundles the in-memory store, the wired orderbook + matching
// keepers, and the stub dependencies so individual tests can drive a
// matching loop with three or four lines of setup.
type matchEnv struct {
	ctx sdk.Context
	ak  *stubAccount
	tk  *stubTrade
	bk  orderbookkeeper.Keeper
	k   matchingkeeper.Keeper
	mk  *stubMarket
}

func newMatchEnv(t *testing.T) *matchEnv {
	t.Helper()
	keys := storetypes.NewKVStoreKeys(orderbooktypes.StoreKey, matchingtypes.StoreKey)
	cdc := moduletestutil.MakeTestEncodingConfig().Codec
	cms := integration.CreateMultiStore(keys, log.NewTestLogger(t))
	ctx := sdk.NewContext(cms, cmtprototypes.Header{}, true, log.NewTestLogger(t))

	mk := &stubMarket{}
	bk := orderbookkeeper.NewKeeper(
		cdc,
		runtime.NewKVStoreService(keys[orderbooktypes.StoreKey]),
		"px1xqcnyve5x5mrwwpev93xxer9venks6t29ke4l8",
		mk,
		stubLocker{},
	)

	ak := newStubAccount()
	tk := &stubTrade{ak: ak}
	k := matchingkeeper.NewKeeper(
		cdc,
		runtime.NewKVStoreService(keys[matchingtypes.StoreKey]),
		"px1xqcnyve5x5mrwwpev93xxer9venks6t29ke4l8",
		ak,
		mk,
		bk,
		tk,
	)
	require.NoError(t, k.Params.Set(ctx, matchingtypes.Params{MaxFillsPerMsg: 64, MaxCancelsPerMsg: 128}))

	return &matchEnv{ctx: ctx, ak: ak, tk: tk, bk: bk, k: k, mk: mk}
}

// rest places a maker order on the book through the public OpenOrder
// lifecycle, mirroring how msgServer.CreateOrder does it. The `isAsk`
// argument is preserved for callsite readability — OpenOrder reads the
// side from o.IsAsk.
func (e *matchEnv) rest(t *testing.T, o orderbooktypes.Order, _ bool) {
	t.Helper()
	require.NoError(t, e.bk.OpenOrder(e.ctx, o, false))
}

// withInjectingTrade returns an env whose tradeKeeper consumes a script
// of injected errors, then falls back to the regular stubTrade behaviour
// for any remaining fills. The keeper exposes `SetTradeKeeper` so the
// external test package can swap the trade keeper without touching the
// (unexported) field directly.
func withInjectingTrade(t *testing.T, errs ...error) (*matchEnv, *injectingTrade) {
	t.Helper()
	e := newMatchEnv(t)
	inj := &injectingTrade{stubTrade: e.tk, errs: errs}
	e.k.SetTradeKeeper(inj)
	return e, inj
}

// --- order builders -------------------------------------------------------

// makeMaker / makeTaker are tiny helpers to keep test bodies readable.
// Both pin MarketIndex=1 and LimitOrder+GTT; tests that need other
// shapes should construct the Order literal inline.
func makeMaker(idx, owner uint64, price uint32, base uint64, isAsk bool, nonce int64) orderbooktypes.Order {
	return orderbooktypes.Order{
		OrderIndex:          idx,
		OwnerAccountIndex:   owner,
		MarketIndex:         1,
		IsAsk:               isAsk,
		OrderType:           perptypes.LimitOrder,
		TimeInForce:         perptypes.GTT,
		Price:               price,
		Nonce:               nonce,
		InitialBaseAmount:   base,
		RemainingBaseAmount: base,
		Status:              perptypes.OrderStatusOpen,
	}
}

func makeTaker(idx, owner uint64, price uint32, base uint64, isAsk bool) *orderbooktypes.Order {
	return &orderbooktypes.Order{
		OrderIndex:          idx,
		OwnerAccountIndex:   owner,
		MarketIndex:         1,
		IsAsk:               isAsk,
		OrderType:           perptypes.LimitOrder,
		TimeInForce:         perptypes.GTT,
		Price:               price,
		Nonce:               int64(idx),
		InitialBaseAmount:   base,
		RemainingBaseAmount: base,
		Status:              perptypes.OrderStatusOpen,
	}
}
