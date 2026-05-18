// spot_lifecycle_test.go wires the real x/orderbook + x/trade + x/matching
// keepers against a high-fidelity spot account stub and exercises the
// full spot order lifecycle:
//
//   OpenOrder (lock-on-place) → partial fill (lock decrement + balance
//   transfer + RemainingBase decrement) → CancelOrder (lock release).
//
// The point of this test is NOT to re-cover any of the per-module unit
// tests but to assert the *cross-module invariant*: that
//
//   x/account.LockedBalance == Order.RemainingBaseAmount * Price
//
// (for a bid; `== RemainingBaseAmount` for an ask) holds at every step
// of the lifecycle. Any future change that breaks the alignment between
// `x/orderbook.computeSpotLock` and `x/trade.spotMakerDebit` —
// fee rounding, slippage applied to the lock formula, partial-fill
// quote math — will be caught here.
//
// The `spotAccount` stub mirrors the real `x/account` semantics
// (drainLockedFirst clamp, IncreaseLockedBalance reserves against
// Balance−Locked, DecreaseLockedBalance clamps to LockedBalance) so the
// stub itself doesn't paper over arithmetic divergences.
package tests

import (
	"context"
	"fmt"
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
	risktypes "github.com/perpdex/perpdex-l1/x/risk/types"
	tradekeeper "github.com/perpdex/perpdex-l1/x/trade/keeper"
	tradetypes "github.com/perpdex/perpdex-l1/x/trade/types"
)

// --- spot account ---------------------------------------------------------

// spotAccount implements all three keeper surfaces required by a spot
// roundtrip — x/matching.AccountKeeper, x/trade.AccountKeeper, and
// x/orderbook.SpotLocker — with semantics that mirror the production
// x/account.Keeper:
//
//   - Balance and LockedBalance are tracked separately
//   - AvailableBalance = Balance − LockedBalance
//   - IncreaseLockedBalance fails when Available < amount
//   - DecreaseLockedBalance clamps to current LockedBalance (lenient)
//   - TransferAccountAssetBalance with drainLockedFirst=true consumes
//     LockedBalance first (clamped), then Balance for the amount
//
// Tests can seed initial balances via `credit()` and inspect either
// component via `balanceOf()` / `lockedOf()`.
type spotAccount struct {
	balance map[[2]uint64]math.Int
	locked  map[[2]uint64]math.Int
}

func newSpotAccount() *spotAccount {
	return &spotAccount{
		balance: map[[2]uint64]math.Int{},
		locked:  map[[2]uint64]math.Int{},
	}
}

func assetKey(acc uint64, asset uint32) [2]uint64 { return [2]uint64{acc, uint64(asset)} }

func (s *spotAccount) credit(acc uint64, asset uint32, amount int64) {
	k := assetKey(acc, asset)
	cur, ok := s.balance[k]
	if !ok {
		cur = math.ZeroInt()
	}
	s.balance[k] = cur.Add(math.NewInt(amount))
}

func (s *spotAccount) balanceOf(acc uint64, asset uint32) math.Int {
	if v, ok := s.balance[assetKey(acc, asset)]; ok {
		return v
	}
	return math.ZeroInt()
}

func (s *spotAccount) lockedOf(acc uint64, asset uint32) math.Int {
	if v, ok := s.locked[assetKey(acc, asset)]; ok {
		return v
	}
	return math.ZeroInt()
}

func (s *spotAccount) GetAccount(_ context.Context, idx uint64) (accounttypes.Account, error) {
	return accounttypes.Account{
		AccountIndex: idx,
		AccountType:  perptypes.MasterAccountType,
		Collateral:   math.ZeroInt(),
	}, nil
}

func (s *spotAccount) GetPosition(_ context.Context, acc uint64, mkt uint32) (accounttypes.AccountPosition, error) {
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

// ApplyFill / AdjustAllocatedMargin implement the perp-position
// surface of the AccountKeeper interface in x/trade/types. The
// spot-matching test only drives spot fills so the trade engine's
// perp position path is never reached against this stub; the helpers
// are stubbed out to satisfy the interface.
func (s *spotAccount) ApplyFill(
	ctx context.Context, accIdx uint64, marketIdx uint32,
	_ uint32, _ uint64, _ int64, _ math.Int,
) (accounttypes.FillApplyResult, error) {
	pos, _ := s.GetPosition(ctx, accIdx, marketIdx)
	return accounttypes.FillApplyResult{Old: pos, New: pos, RealizedPnL: math.ZeroInt()}, nil
}

func (s *spotAccount) AdjustAllocatedMargin(
	ctx context.Context, accIdx uint64, marketIdx uint32, _ math.Int,
) (accounttypes.AccountPosition, error) {
	return s.GetPosition(ctx, accIdx, marketIdx)
}

func (s *spotAccount) IsAuthorized(_ context.Context, _ string, _ uint64) (bool, error) {
	return true, nil
}

func (s *spotAccount) AvailableBalance(_ context.Context, acc uint64, asset uint32) (math.Int, error) {
	avail := s.balanceOf(acc, asset).Sub(s.lockedOf(acc, asset))
	if avail.IsNegative() {
		return math.ZeroInt(), nil
	}
	return avail, nil
}

func (s *spotAccount) AddCollateral(_ context.Context, _ uint64, _ math.Int) error { return nil }

func (s *spotAccount) GetAccountAsset(_ context.Context, acc uint64, asset uint32) (accounttypes.AccountAsset, error) {
	return accounttypes.AccountAsset{
		AccountIndex:  acc,
		AssetIndex:    asset,
		Balance:       s.balanceOf(acc, asset),
		LockedBalance: s.lockedOf(acc, asset),
	}, nil
}

// IncreaseLockedBalance reserves `amount` against (acc, asset). Mirrors
// the production lock-on-place check: fails when available is short.
func (s *spotAccount) IncreaseLockedBalance(_ context.Context, acc uint64, asset uint32, amount math.Int) error {
	if amount.IsNil() || amount.IsZero() {
		return nil
	}
	if amount.IsNegative() {
		return accounttypes.ErrInsufficientFunds.Wrap("lock amount must be non-negative")
	}
	bal := s.balanceOf(acc, asset)
	loc := s.lockedOf(acc, asset)
	avail := bal.Sub(loc)
	if avail.LT(amount) {
		return accounttypes.ErrInsufficientFunds.Wrapf(
			"account %d asset %d available %s need %s",
			acc, asset, avail.String(), amount.String(),
		)
	}
	s.locked[assetKey(acc, asset)] = loc.Add(amount)
	return nil
}

// DecreaseLockedBalance is lenient: it clamps `amount` to the current
// LockedBalance — releasing more than what was locked is a no-op for
// the excess (matches production behaviour relied on by the orderbook
// cancel path).
func (s *spotAccount) DecreaseLockedBalance(_ context.Context, acc uint64, asset uint32, amount math.Int) error {
	if amount.IsNil() || amount.IsZero() {
		return nil
	}
	if amount.IsNegative() {
		return accounttypes.ErrInsufficientFunds.Wrap("release amount must be non-negative")
	}
	loc := s.lockedOf(acc, asset)
	release := amount
	if release.GT(loc) {
		release = loc
	}
	s.locked[assetKey(acc, asset)] = loc.Sub(release)
	return nil
}

// TransferAccountAssetBalance mirrors the production semantics: with
// drainLockedFirst=true it consumes LockedBalance first (clamped),
// then debits Balance for the full amount; with drainLockedFirst=false
// it requires available headroom (Balance − Locked >= amount).
func (s *spotAccount) TransferAccountAssetBalance(
	_ context.Context, from, to uint64, asset uint32, amount math.Int, drainLockedFirst bool,
) error {
	if amount.IsNil() || amount.IsZero() {
		return nil
	}
	if amount.IsNegative() {
		return accounttypes.ErrInsufficientFunds.Wrap("transfer amount must be non-negative")
	}
	bal := s.balanceOf(from, asset)
	loc := s.lockedOf(from, asset)
	if drainLockedFirst {
		if bal.LT(amount) {
			return accounttypes.ErrInsufficientFunds.Wrapf(
				"account %d asset %d have %s need %s",
				from, asset, bal.String(), amount.String(),
			)
		}
		drain := amount
		if drain.GT(loc) {
			drain = loc
		}
		s.locked[assetKey(from, asset)] = loc.Sub(drain)
	} else {
		avail := bal.Sub(loc)
		if avail.LT(amount) {
			return accounttypes.ErrInsufficientFunds.Wrapf(
				"account %d asset %d available %s need %s",
				from, asset, avail.String(), amount.String(),
			)
		}
	}
	s.balance[assetKey(from, asset)] = bal.Sub(amount)
	dst := s.balanceOf(to, asset)
	s.balance[assetKey(to, asset)] = dst.Add(amount)
	return nil
}

// --- spot market ----------------------------------------------------------

// spotMarketStub returns a SPOT market with configurable fees and the
// canonical base/quote asset IDs the test fixture uses.
type spotMarketStub struct {
	baseAsset uint32
	quoteAsset uint32
	takerFee  uint32
	makerFee  uint32
	maxOpen   uint32

	nonceAsk int64
	nonceBid int64

	oi map[uint32]int64
}

const (
	spotMarketIndex      = uint32(2048) // MinSpotMarketIndex
	spotMarketBaseAsset  = uint32(42)
	spotMarketQuoteAsset = uint32(43)
)

func newSpotMarketStub() *spotMarketStub {
	return &spotMarketStub{
		baseAsset:  spotMarketBaseAsset,
		quoteAsset: spotMarketQuoteAsset,
		maxOpen:    16,
		oi:         map[uint32]int64{},
	}
}

func (s *spotMarketStub) GetMarket(_ context.Context, idx uint32) (markettypes.Market, error) {
	return markettypes.Market{
		MarketIndex:             idx,
		MarketType:              perptypes.MarketTypeSpot,
		Status:                  perptypes.MarketStatusActive,
		BaseAssetId:             s.baseAsset,
		QuoteAssetId:            s.quoteAsset,
		TakerFee:                s.takerFee,
		MakerFee:                s.makerFee,
		MaxOpenOrdersPerAccount: s.maxOpen,
	}, nil
}

func (s *spotMarketStub) GetMarketDetails(_ context.Context, idx uint32) (markettypes.MarketDetails, error) {
	return markettypes.MarketDetails{MarketIndex: idx, MarkPrice: 1, LastMarkPriceRefreshTimestamp: 1}, nil
}

func (s *spotMarketStub) GetMarkPriceAndDetails(_ context.Context, idx uint32) (uint32, markettypes.MarketDetails, error) {
	return 1, markettypes.MarketDetails{MarketIndex: idx, MarkPrice: 1, LastMarkPriceRefreshTimestamp: 1}, nil
}

func (s *spotMarketStub) AllocateNonce(_ context.Context, _ uint32, isAsk bool) (int64, error) {
	if isAsk {
		s.nonceAsk++
		return s.nonceAsk, nil
	}
	s.nonceBid--
	return s.nonceBid, nil
}

func (s *spotMarketStub) UpdateOpenInterest(_ context.Context, idx uint32, delta int64) error {
	s.oi[idx] += delta
	return nil
}

// --- funding / risk stubs (no-op for spot) --------------------------------

type spotFundingStub struct{}

func (spotFundingStub) SettlePositionFunding(_ context.Context, _ uint64, _ uint32) error {
	return nil
}

type spotRiskStub struct{}

func (spotRiskStub) SnapshotRisk(_ context.Context, _ uint64) (risktypes.PreRiskSnapshot, error) {
	return risktypes.PreRiskSnapshot{}, nil
}

func (spotRiskStub) IsValidRiskChangeFrom(_ context.Context, _ uint64, _ risktypes.PreRiskSnapshot) (bool, error) {
	return true, nil
}

func (spotRiskStub) GetAvailableUsdcCollateral(_ context.Context, _ uint64) (math.Int, error) {
	return math.NewIntFromUint64(1 << 62), nil
}

// --- spot env -------------------------------------------------------------

type spotEnv struct {
	ctx sdk.Context
	ak  *spotAccount
	mk  *spotMarketStub
	bk  orderbookkeeper.Keeper
	tk  tradekeeper.Keeper
	k   matchingkeeper.Keeper
}

func newSpotEnv(t *testing.T) *spotEnv {
	t.Helper()
	keys := storetypes.NewKVStoreKeys(orderbooktypes.StoreKey, matchingtypes.StoreKey, tradetypes.StoreKey)
	cdc := moduletestutil.MakeTestEncodingConfig().Codec
	cms := integration.CreateMultiStore(keys, log.NewTestLogger(t))
	ctx := sdk.NewContext(cms, cmtprototypes.Header{}, true, log.NewTestLogger(t))

	ak := newSpotAccount()
	mk := newSpotMarketStub()

	bk := orderbookkeeper.NewKeeper(
		cdc,
		runtime.NewKVStoreService(keys[orderbooktypes.StoreKey]),
		"px1xqcnyve5x5mrwwpev93xxer9venks6t29ke4l8",
		mk,
		ak,
	)

	tk := tradekeeper.NewKeeper(
		cdc,
		runtime.NewKVStoreService(keys[tradetypes.StoreKey]),
		"px1xqcnyve5x5mrwwpev93xxer9venks6t29ke4l8",
		ak,
		mk,
		spotFundingStub{},
		spotRiskStub{},
	)

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

	return &spotEnv{ctx: ctx, ak: ak, mk: mk, bk: bk, tk: tk, k: k}
}

// requireIntEqual is a math.Int-aware require.Equal: it compares by
// numeric value (math.Int.Equal) rather than struct identity, so
// `math.ZeroInt()` (abs=nil) and a zero produced via Sub (abs=empty
// big.nat slice) compare equal as they should.
func requireIntEqual(t *testing.T, want, got math.Int, msgAndArgs ...any) {
	t.Helper()
	require.True(t, want.Equal(got),
		"want=%s got=%s%s", want.String(), got.String(),
		formatExtra(msgAndArgs...))
}

func formatExtra(msgAndArgs ...any) string {
	if len(msgAndArgs) == 0 {
		return ""
	}
	if s, ok := msgAndArgs[0].(string); ok && len(msgAndArgs) == 1 {
		return " — " + s
	}
	return fmt.Sprintf(" — %v", msgAndArgs)
}

// requireInvariant asserts the cross-module lock alignment:
//
//	bid maker: locked(quote) == remaining_base * price
//	ask maker: locked(base)  == remaining_base
//
// Called at every step of the lifecycle so a regression in either side
// (computeSpotLock or spotMakerDebit) fails immediately.
func (e *spotEnv) requireInvariant(t *testing.T, orderIndex uint64, expectExists bool) {
	t.Helper()
	o, err := e.bk.GetOrder(e.ctx, orderIndex)
	if !expectExists {
		require.Error(t, err, "order %d should have been removed", orderIndex)
		return
	}
	require.NoError(t, err)
	var (
		asset    uint32
		expected math.Int
	)
	if o.IsAsk {
		asset = spotMarketBaseAsset
		expected = math.NewIntFromUint64(o.RemainingBaseAmount)
	} else {
		asset = spotMarketQuoteAsset
		expected = math.NewIntFromUint64(o.RemainingBaseAmount).
			Mul(math.NewIntFromUint64(uint64(o.Price)))
	}
	got := e.ak.lockedOf(o.OwnerAccountIndex, asset)
	require.True(t, expected.Equal(got),
		fmt.Sprintf("lock invariant broken for order %d: expected locked=%s got %s (remaining=%d price=%d isAsk=%v)",
			orderIndex, expected.String(), got.String(), o.RemainingBaseAmount, o.Price, o.IsAsk))
}

// --- tests ----------------------------------------------------------------

// TestSpot_BidLifecycle_PartialFillThenCancel walks the canonical
// happy path on the bid side:
//   t0: maker has 10_000 quote.
//   t1: maker rests bid (price=100, base=10) → locked = 1_000 quote.
//   t2: taker sells 4 base → fill of 4 @ 100.
//       maker locked drops to 600 quote, balance drops by 400 quote,
//       base balance climbs to 4, taker quote balance climbs to 400.
//   t3: maker cancels residue (6 base @ 100 = 600 quote) → locked=0,
//       quote balance fully restored to 10_000 − 400 = 9_600.
func TestSpot_BidLifecycle_PartialFillThenCancel(t *testing.T) {
	e := newSpotEnv(t)
	const (
		makerAcc uint64 = 11
		takerAcc uint64 = 22
		price    uint32 = 100
		makerSz  uint64 = 10
		fill     uint64 = 4
	)
	e.ak.credit(makerAcc, spotMarketQuoteAsset, 10_000)
	e.ak.credit(takerAcc, spotMarketBaseAsset, 10)

	maker := orderbooktypes.Order{
		OrderIndex:          1,
		OwnerAccountIndex:   makerAcc,
		MarketIndex:         spotMarketIndex,
		IsAsk:               false,
		OrderType:           perptypes.LimitOrder,
		TimeInForce:         perptypes.GTT,
		Price:               price,
		Nonce:               -1,
		InitialBaseAmount:   makerSz,
		RemainingBaseAmount: makerSz,
		Status:              perptypes.OrderStatusOpen,
	}
	require.NoError(t, e.bk.OpenOrder(e.ctx, maker))
	requireIntEqual(t, math.NewInt(int64(makerSz)*int64(price)), e.ak.lockedOf(makerAcc, spotMarketQuoteAsset))
	e.requireInvariant(t, maker.OrderIndex, true)

	taker := &orderbooktypes.Order{
		OrderIndex:          2,
		OwnerAccountIndex:   takerAcc,
		MarketIndex:         spotMarketIndex,
		IsAsk:               true,
		OrderType:           perptypes.LimitOrder,
		TimeInForce:         perptypes.IOC,
		Price:               price,
		Nonce:               1,
		InitialBaseAmount:   fill,
		RemainingBaseAmount: fill,
		Status:              perptypes.OrderStatusOpen,
	}
	filled, status, err := e.k.MatchOrder(e.ctx, taker, 16)
	require.NoError(t, err)
	require.Equal(t, fill, filled)
	require.Equal(t, perptypes.OrderStatusFilled, status)

	requireIntEqual(t, math.NewInt(int64(makerSz-fill)*int64(price)),
		e.ak.lockedOf(makerAcc, spotMarketQuoteAsset),
		"maker locked should drop by fill*price after spotMakerDebit drains the lock")
	requireIntEqual(t, math.NewInt(10_000-int64(fill)*int64(price)),
		e.ak.balanceOf(makerAcc, spotMarketQuoteAsset),
		"maker quote balance must drop by exactly the matched notional")
	requireIntEqual(t, math.NewInt(int64(fill)),
		e.ak.balanceOf(makerAcc, spotMarketBaseAsset),
		"maker should have received base equal to fill")
	requireIntEqual(t, math.NewInt(int64(fill)*int64(price)),
		e.ak.balanceOf(takerAcc, spotMarketQuoteAsset),
		"taker should have received quote equal to matched notional")
	requireIntEqual(t, math.NewInt(10-int64(fill)),
		e.ak.balanceOf(takerAcc, spotMarketBaseAsset),
		"taker base balance must drop by the filled base amount")

	residue, err := e.bk.GetOrder(e.ctx, maker.OrderIndex)
	require.NoError(t, err)
	require.Equal(t, makerSz-fill, residue.RemainingBaseAmount)
	require.Equal(t, perptypes.OrderStatusPartiallyFilled, residue.Status)
	e.requireInvariant(t, maker.OrderIndex, true)

	_, err = e.bk.CancelOrder(e.ctx, maker.OrderIndex)
	require.NoError(t, err)
	requireIntEqual(t, math.ZeroInt(), e.ak.lockedOf(makerAcc, spotMarketQuoteAsset),
		"lock must be fully released after cancel")
	requireIntEqual(t, math.NewInt(10_000-int64(fill)*int64(price)),
		e.ak.balanceOf(makerAcc, spotMarketQuoteAsset),
		"maker quote balance must equal the pre-trade balance minus filled notional")
}

// TestSpot_AskLifecycle_PartialFillThenCancel is the mirror image of
// the bid case on the ask side:
//   t0: maker has 10 base.
//   t1: maker rests ask (price=200, base=10) → locked = 10 base.
//   t2: taker buys 3 base @ 200 → maker locked=7 base, quote climbs 600,
//       taker pays 600 quote, taker receives 3 base.
//   t3: cancel residue → maker locked=0; maker still owns 7 base.
func TestSpot_AskLifecycle_PartialFillThenCancel(t *testing.T) {
	e := newSpotEnv(t)
	const (
		makerAcc uint64 = 31
		takerAcc uint64 = 32
		price    uint32 = 200
		makerSz  uint64 = 10
		fill     uint64 = 3
	)
	e.ak.credit(makerAcc, spotMarketBaseAsset, int64(makerSz))
	e.ak.credit(takerAcc, spotMarketQuoteAsset, 10_000)

	maker := orderbooktypes.Order{
		OrderIndex:          10,
		OwnerAccountIndex:   makerAcc,
		MarketIndex:         spotMarketIndex,
		IsAsk:               true,
		OrderType:           perptypes.LimitOrder,
		TimeInForce:         perptypes.GTT,
		Price:               price,
		Nonce:               1,
		InitialBaseAmount:   makerSz,
		RemainingBaseAmount: makerSz,
		Status:              perptypes.OrderStatusOpen,
	}
	require.NoError(t, e.bk.OpenOrder(e.ctx, maker))
	requireIntEqual(t, math.NewInt(int64(makerSz)),
		e.ak.lockedOf(makerAcc, spotMarketBaseAsset),
		"ask maker locks remaining_base of the base asset")
	e.requireInvariant(t, maker.OrderIndex, true)

	taker := &orderbooktypes.Order{
		OrderIndex:          11,
		OwnerAccountIndex:   takerAcc,
		MarketIndex:         spotMarketIndex,
		IsAsk:               false,
		OrderType:           perptypes.LimitOrder,
		TimeInForce:         perptypes.IOC,
		Price:               price,
		Nonce:               -1,
		InitialBaseAmount:   fill,
		RemainingBaseAmount: fill,
		Status:              perptypes.OrderStatusOpen,
	}
	filled, status, err := e.k.MatchOrder(e.ctx, taker, 16)
	require.NoError(t, err)
	require.Equal(t, fill, filled)
	require.Equal(t, perptypes.OrderStatusFilled, status)

	requireIntEqual(t, math.NewInt(int64(makerSz-fill)),
		e.ak.lockedOf(makerAcc, spotMarketBaseAsset),
		"maker base lock should drop by exactly the matched base")
	requireIntEqual(t, math.NewInt(int64(makerSz-fill)),
		e.ak.balanceOf(makerAcc, spotMarketBaseAsset),
		"maker base balance must reflect the post-trade residue")
	requireIntEqual(t, math.NewInt(int64(fill)*int64(price)),
		e.ak.balanceOf(makerAcc, spotMarketQuoteAsset),
		"maker should have received quote equal to matched notional")
	requireIntEqual(t, math.NewInt(int64(fill)),
		e.ak.balanceOf(takerAcc, spotMarketBaseAsset),
		"taker base must climb by exactly the filled base")
	requireIntEqual(t, math.NewInt(10_000-int64(fill)*int64(price)),
		e.ak.balanceOf(takerAcc, spotMarketQuoteAsset),
		"taker quote must drop by exactly the matched notional")

	residue, err := e.bk.GetOrder(e.ctx, maker.OrderIndex)
	require.NoError(t, err)
	require.Equal(t, makerSz-fill, residue.RemainingBaseAmount)
	require.Equal(t, perptypes.OrderStatusPartiallyFilled, residue.Status)
	e.requireInvariant(t, maker.OrderIndex, true)

	_, err = e.bk.CancelOrder(e.ctx, maker.OrderIndex)
	require.NoError(t, err)
	requireIntEqual(t, math.ZeroInt(), e.ak.lockedOf(makerAcc, spotMarketBaseAsset))
	requireIntEqual(t, math.NewInt(int64(makerSz-fill)), e.ak.balanceOf(makerAcc, spotMarketBaseAsset))
}

// TestSpot_FullFillTerminatesAndUnlocks tightens the round-trip on
// the fully-filled path: after a maker is exactly cleared, its
// LockedBalance must be zero and the Order record must terminate at
// Filled — there is no residue to cancel.
func TestSpot_FullFillTerminatesAndUnlocks(t *testing.T) {
	e := newSpotEnv(t)
	const (
		makerAcc uint64 = 41
		takerAcc uint64 = 42
		price    uint32 = 50
		size     uint64 = 5
	)
	e.ak.credit(makerAcc, spotMarketQuoteAsset, 1_000)
	e.ak.credit(takerAcc, spotMarketBaseAsset, int64(size))

	maker := orderbooktypes.Order{
		OrderIndex:          100,
		OwnerAccountIndex:   makerAcc,
		MarketIndex:         spotMarketIndex,
		IsAsk:               false,
		OrderType:           perptypes.LimitOrder,
		TimeInForce:         perptypes.GTT,
		Price:               price,
		Nonce:               -1,
		InitialBaseAmount:   size,
		RemainingBaseAmount: size,
		Status:              perptypes.OrderStatusOpen,
	}
	require.NoError(t, e.bk.OpenOrder(e.ctx, maker))
	e.requireInvariant(t, maker.OrderIndex, true)

	taker := &orderbooktypes.Order{
		OrderIndex:          101,
		OwnerAccountIndex:   takerAcc,
		MarketIndex:         spotMarketIndex,
		IsAsk:               true,
		OrderType:           perptypes.LimitOrder,
		TimeInForce:         perptypes.IOC,
		Price:               price,
		Nonce:               1,
		InitialBaseAmount:   size,
		RemainingBaseAmount: size,
		Status:              perptypes.OrderStatusOpen,
	}
	_, status, err := e.k.MatchOrder(e.ctx, taker, 16)
	require.NoError(t, err)
	require.Equal(t, perptypes.OrderStatusFilled, status)

	requireIntEqual(t, math.ZeroInt(), e.ak.lockedOf(makerAcc, spotMarketQuoteAsset),
		"a fully-filled maker must release its lock down to zero")

	terminated, err := e.bk.GetOrder(e.ctx, maker.OrderIndex)
	require.NoError(t, err)
	require.Equal(t, perptypes.OrderStatusFilled, terminated.Status)
	require.Zero(t, terminated.RemainingBaseAmount)
}
