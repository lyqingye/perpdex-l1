package keeper

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"

	perptypes "github.com/perpdex/perpdex-l1/types"
	orderbooktypes "github.com/perpdex/perpdex-l1/x/orderbook/types"
	tradetypes "github.com/perpdex/perpdex-l1/x/trade/types"
)

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

// TestMatchLiquidation_PerpFillCarriesLiquidationFields exercises the
// end-to-end plumbing from the matching keeper's
// `MatchLiquidationOrder` entry point through `matchLiquidation`
// down to the trade keeper invocation. It verifies that the resulting
// PerpFill has:
//
//   - MakerAccountIndex = the resting bid maker
//   - TakerAccountIndex = the victim
//   - Price             = maker price (NOT zero price; matching engine
//     fills at maker prices that improve the
//     zero-price floor)
//   - TakerFee/MakerFee = 0 (only liquidation improvement fee applies)
//   - ZeroPrice         = forwarded from MatchLiquidationOrder arg
//   - LiquidationFeeBps = forwarded from MatchLiquidationOrder arg
//   - LiquidationFeeRecipient = forwarded from MatchLiquidationOrder arg
//   - NoRiskCheck       = false; both sides go through IsValidRiskChange
//     post-trade, mirroring Lighter
//     `matching_engine.rs:1801,1843` for liquidation orders. Recoverable
//     rejections flow through errMakerRejected / errTakerRejected.
//
// This is the matching-keeper-side contract for the
// "PARTIAL_LIQUIDATION goes through the orderbook IOC" alignment with
// Lighter.
func TestMatchLiquidation_PerpFillCarriesLiquidationFields(t *testing.T) {
	e := newMatchEnv(t)
	e.k.SetRiskKeeper(newStubRisk())

	const (
		victim    = uint64(100)
		makerAcc  = uint64(200)
		market    = uint32(1)
		zeroPrice = uint32(100)
		makerBid  = uint32(110) // strictly better than zero price
		qty       = uint64(50)
		feeBps    = uint32(10_000)
		llpIdx    = perptypes.InsuranceFundOperatorAccountIdx
	)
	// Victim is long; close-out is a SELL (taker ask).
	e.ak.setPosition(victim, market, int64(qty))

	bid := orderbooktypes.Order{
		OrderIndex:          1,
		OwnerAccountIndex:   makerAcc,
		MarketIndex:         market,
		IsAsk:               false,
		OrderType:           perptypes.LimitOrder,
		TimeInForce:         perptypes.GTT,
		Price:               makerBid,
		Nonce:               1,
		InitialBaseAmount:   qty,
		RemainingBaseAmount: qty,
		Status:              perptypes.OrderStatusOpen,
	}
	e.rest(t, bid, false)

	filled, err := e.k.MatchLiquidationOrder(
		e.ctx, victim, market, zeroPrice, qty, feeBps, llpIdx,
	)
	require.NoError(t, err)
	require.EqualValues(t, qty, filled, "IOC must fully consume the resting bid")
	require.Len(t, e.tk.fills, 1)
	got := e.tk.fills[0]
	require.Equal(t, makerAcc, got.MakerAccountIndex)
	require.Equal(t, victim, got.TakerAccountIndex)
	require.Equal(t, market, got.MarketIndex)
	require.Equal(t, makerBid, got.Price, "fill must commit at maker price, not zero price")
	require.True(t, got.IsTakerAsk, "long victim closes via taker ask")
	require.Equal(t, uint32(0), got.TakerFee)
	require.Equal(t, uint32(0), got.MakerFee)
	require.Equal(t, zeroPrice, got.ZeroPrice)
	require.Equal(t, feeBps, got.LiquidationFeeBps)
	require.Equal(t, llpIdx, got.LiquidationFeeRecipient)
	require.False(t, got.NoRiskCheck,
		"liquidation fill must validate both maker and taker post-state risk (Lighter parity)")
	require.False(t, got.SkipMakerRiskCheck,
		"liquidation fill must validate maker risk change")
	require.False(t, got.SkipTakerRiskCheck,
		"liquidation fill must validate taker risk change")
}

// TestMatchLiquidation_HealthShortCircuit covers the Lighter
// `is_not_in_liquidation_and_is_liquidation_order` short-circuit:
// after the first fill the victim's health recovers, so the loop
// must STOP consuming the book even though there is still a second
// maker willing to trade and the IOC still has remaining base.
func TestMatchLiquidation_HealthShortCircuit(t *testing.T) {
	e := newMatchEnv(t)
	rk := newStubRisk()
	rk.defaultStatus = perptypes.HealthHealthy
	rk.cross[100] = []uint32{perptypes.HealthHealthy} // post-fill = HEALTHY
	e.k.SetRiskKeeper(rk)

	const (
		victim     = uint64(100)
		bidder1Acc = uint64(200)
		bidder2Acc = uint64(201)
		market     = uint32(1)
		zeroPrice  = uint32(100)
	)
	// Long victim of size 20 — enough to consume both bidders if the
	// short-circuit didn't fire.
	e.ak.setPosition(victim, market, 20)

	bid1 := orderbooktypes.Order{
		OrderIndex: 1, OwnerAccountIndex: bidder1Acc, MarketIndex: market,
		IsAsk: false, OrderType: perptypes.LimitOrder, TimeInForce: perptypes.GTT,
		Price: 110, Nonce: 1,
		InitialBaseAmount: 5, RemainingBaseAmount: 5,
		Status: perptypes.OrderStatusOpen,
	}
	bid2 := orderbooktypes.Order{
		OrderIndex: 2, OwnerAccountIndex: bidder2Acc, MarketIndex: market,
		IsAsk: false, OrderType: perptypes.LimitOrder, TimeInForce: perptypes.GTT,
		Price: 105, Nonce: 2,
		InitialBaseAmount: 15, RemainingBaseAmount: 15,
		Status: perptypes.OrderStatusOpen,
	}
	e.rest(t, bid1, false)
	e.rest(t, bid2, false)

	filled, err := e.k.MatchLiquidationOrder(e.ctx, victim, market, zeroPrice, 20, 10_000, perptypes.InsuranceFundOperatorAccountIdx)
	require.NoError(t, err)
	require.EqualValues(t, 5, filled,
		"only the first maker should fill; the loop must short-circuit when victim recovers")
	require.Len(t, e.tk.fills, 1, "short-circuit must abort before second maker is consumed")

	b2, err := e.bk.GetOrder(e.ctx, 2)
	require.NoError(t, err)
	require.Equal(t, perptypes.OrderStatusOpen, b2.Status,
		"second bid must remain on the book — the IOC residue is dropped, not matched")
}

// TestMatchLiquidation_PriceUnreachableBreaksImmediately verifies the
// price-reachable guard: a long-victim IOC at zero_price=100 against a
// resting BID at 90 must NOT fill (90 < 100 means selling there would
// violate the zero-price floor). The IOC simply terminates with
// filled=0 and the bid stays untouched.
func TestMatchLiquidation_PriceUnreachableBreaksImmediately(t *testing.T) {
	e := newMatchEnv(t)
	e.k.SetRiskKeeper(newStubRisk())

	e.ak.setPosition(100, 1, 10)
	bid := orderbooktypes.Order{
		OrderIndex: 1, OwnerAccountIndex: 200, MarketIndex: 1,
		IsAsk: false, OrderType: perptypes.LimitOrder, TimeInForce: perptypes.GTT,
		Price: 90, Nonce: 1,
		InitialBaseAmount: 10, RemainingBaseAmount: 10,
		Status: perptypes.OrderStatusOpen,
	}
	e.rest(t, bid, false)

	filled, err := e.k.MatchLiquidationOrder(e.ctx, 100, 1, /*zeroPrice=*/ 100, 10, 10_000, perptypes.InsuranceFundOperatorAccountIdx)
	require.NoError(t, err)
	require.Zero(t, filled,
		"IOC must not fill below the zero-price floor")
	require.Empty(t, e.tk.fills)

	b, err := e.bk.GetOrder(e.ctx, 1)
	require.NoError(t, err)
	require.Equal(t, perptypes.OrderStatusOpen, b.Status,
		"unreachable bid must stay resting on the book")

	// The synthetic IOC taker is never persisted to the orderbook —
	// even when filled is 0, the victim's open-order count must stay
	// at 0 because MatchLiquidationOrder skips OpenOrder entirely.
	cnt, err := e.bk.GetAccountOpenOrderCount(e.ctx, 100, 1)
	require.NoError(t, err)
	require.Zero(t, cnt,
		"liquidation IOC residue must never enter the orderbook indexes")
}

// TestMatchLiquidation_HealthShortCircuit_Bankruptcy ensures a victim
// who progresses from PARTIAL into BANKRUPTCY between fills is NOT
// short-circuited by `needsLiquidation` — the loop must keep matching
// because BANKRUPTCY is part of Lighter's `is_in_liquidation` set.
// Without Gap A's BANKRUPTCY arm, the second maker would be
// erroneously skipped the moment the victim's health reading flipped
// to BANKRUPTCY mid-loop (e.g., from a funding accrual).
func TestMatchLiquidation_HealthShortCircuit_Bankruptcy(t *testing.T) {
	e := newMatchEnv(t)
	rk := newStubRisk()
	rk.defaultStatus = perptypes.HealthBankruptcy
	// First read after fill 1 reports BANKRUPTCY (still in
	// liquidation per Lighter); fall-back default also BANKRUPTCY.
	rk.cross[100] = []uint32{perptypes.HealthBankruptcy}
	e.k.SetRiskKeeper(rk)

	const (
		victim     = uint64(100)
		bidder1Acc = uint64(200)
		bidder2Acc = uint64(201)
		market     = uint32(1)
		zeroPrice  = uint32(100)
	)
	e.ak.setPosition(victim, market, 20)

	bid1 := orderbooktypes.Order{
		OrderIndex: 1, OwnerAccountIndex: bidder1Acc, MarketIndex: market,
		IsAsk: false, OrderType: perptypes.LimitOrder, TimeInForce: perptypes.GTT,
		Price: 110, Nonce: 1,
		InitialBaseAmount: 5, RemainingBaseAmount: 5,
		Status: perptypes.OrderStatusOpen,
	}
	bid2 := orderbooktypes.Order{
		OrderIndex: 2, OwnerAccountIndex: bidder2Acc, MarketIndex: market,
		IsAsk: false, OrderType: perptypes.LimitOrder, TimeInForce: perptypes.GTT,
		Price: 105, Nonce: 2,
		InitialBaseAmount: 15, RemainingBaseAmount: 15,
		Status: perptypes.OrderStatusOpen,
	}
	e.rest(t, bid1, false)
	e.rest(t, bid2, false)

	filled, err := e.k.MatchLiquidationOrder(
		e.ctx, victim, market, zeroPrice, 20, 10_000,
		perptypes.InsuranceFundOperatorAccountIdx,
	)
	require.NoError(t, err)
	require.EqualValues(t, 20, filled,
		"BANKRUPTCY must remain in the liquidation predicate; loop should drain both makers")
	require.Len(t, e.tk.fills, 2,
		"both makers must be consumed when victim stays in BANKRUPTCY")
}

// TestMatchLiquidation_VictimRiskRegression_StopsGracefully covers
// Gap B's interaction with the matching loop: with the liquidation
// IOC's `NoRiskCheck=false`, a recoverable taker-side risk
// regression (e.g., `ErrTakerInsufficientCollateral`) must abort the
// matching loop *gracefully*, preserving any prior committed fills
// and dropping the IOC residue without persisting the synthetic
// taker. This is the same semantics as Lighter's
// `internal_liquidation.rs` aborting the IOC on victim post-trade
// regression while keeping prior partial fills.
func TestMatchLiquidation_VictimRiskRegression_StopsGracefully(t *testing.T) {
	// First fill commits cleanly; the second fill simulates the
	// victim flunking the post-trade taker risk check (the engine
	// returns ErrTakerInsufficientCollateral, which `applyPerpFill`
	// remaps to `errTakerRejected` for the matching loop).
	e, _ := withInjectingTrade(t, nil, tradetypes.ErrTakerInsufficientCollateral)
	e.k.SetRiskKeeper(newStubRisk()) // default HEALTHY → set victim to FULL_LIQ below
	rk := newStubRisk()
	// Keep the victim "in liquidation" across both checks so the
	// loop does not short-circuit before we reach the second maker.
	rk.defaultStatus = perptypes.HealthFullLiquidation
	e.k.SetRiskKeeper(rk)

	const (
		victim     = uint64(100)
		bidder1Acc = uint64(200)
		bidder2Acc = uint64(201)
		market     = uint32(1)
		zeroPrice  = uint32(100)
	)
	e.ak.setPosition(victim, market, 20)

	bid1 := orderbooktypes.Order{
		OrderIndex: 1, OwnerAccountIndex: bidder1Acc, MarketIndex: market,
		IsAsk: false, OrderType: perptypes.LimitOrder, TimeInForce: perptypes.GTT,
		Price: 110, Nonce: 1,
		InitialBaseAmount: 5, RemainingBaseAmount: 5,
		Status: perptypes.OrderStatusOpen,
	}
	bid2 := orderbooktypes.Order{
		OrderIndex: 2, OwnerAccountIndex: bidder2Acc, MarketIndex: market,
		IsAsk: false, OrderType: perptypes.LimitOrder, TimeInForce: perptypes.GTT,
		Price: 105, Nonce: 2,
		InitialBaseAmount: 15, RemainingBaseAmount: 15,
		Status: perptypes.OrderStatusOpen,
	}
	e.rest(t, bid1, false)
	e.rest(t, bid2, false)

	filled, err := e.k.MatchLiquidationOrder(
		e.ctx, victim, market, zeroPrice, 20, 10_000,
		perptypes.InsuranceFundOperatorAccountIdx,
	)
	require.NoError(t, err,
		"errTakerRejected must surface to the caller as a graceful stop, not an error")
	require.EqualValues(t, 5, filled,
		"only the first fill (5) should commit; the rejected second attempt must not advance `filled`")
	require.Len(t, e.tk.fills, 1,
		"the second maker's attempt was rejected — only the first fill is recorded")

	// Second bid must remain untouched (the loop aborted on taker
	// regression, not on a maker problem).
	b2, err := e.bk.GetOrder(e.ctx, 2)
	require.NoError(t, err)
	require.Equal(t, perptypes.OrderStatusOpen, b2.Status,
		"second bid must remain on the book — the IOC residue is dropped, not matched")

	// IOC residue must never enter the orderbook indexes.
	cnt, err := e.bk.GetAccountOpenOrderCount(e.ctx, victim, market)
	require.NoError(t, err)
	require.Zero(t, cnt,
		"liquidation IOC residue must never persist as an open order")
}
