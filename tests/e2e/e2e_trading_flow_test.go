package e2e_test

import (
	"testing"

	"cosmossdk.io/math"

	perptypes "github.com/perpdex/perpdex-l1/types"

	"github.com/perpdex/perpdex-l1/tests/e2e"
	"github.com/perpdex/perpdex-l1/tests/e2e/msg"
)

// TradingFlowSuite exercises the happy-path lifecycle of a perp market:
// register asset → create market → deposit collateral → cross matching →
// close positions → withdraw collateral. The scenario walks through every
// keeper that participates in a normal flow (asset, market, account,
// orderbook, matching, trade, market again for OI).
type TradingFlowSuite struct {
	e2e.PerpdexTestSuite

	BTCAssetIndex uint32
	MarketIndex   uint32
}

func TestTradingFlowSuite(t *testing.T) {
	e2e.RunSuite(t, new(TradingFlowSuite))
}

// SetupTest layers on top of the base suite by also registering BTC and
// creating the BTC-USD perpetual market once per test, so each TestX
// method starts in a "ready to trade" state.
func (s *TradingFlowSuite) SetupTest() {
	s.PerpdexTestSuite.SetupTest()

	s.BTCAssetIndex = s.RegisterAsset(msg.AssetOpts{
		Denom:               "ubtc",
		DisplayName:         "BTC",
		Decimals:            8,
		ExtensionMultiplier: 1,
		MinTransferAmount:   1,
		MinWithdrawalAmount: 1,
		MarginMode:          perptypes.MarginModeDisabled,
	})

	// Pick a market index in the perp range. 1 is below MaxPerpsMarketIndex
	// and not reserved by other tests.
	s.MarketIndex = s.CreatePerpMarket(msg.DefaultPerpMarketOpts(1, s.BTCAssetIndex))

	// Seed a live oracle price so the risk keeper can classify
	// non-zero positions. Risk fails closed on missing prices, so any
	// test that opens a perp position must seed a mark price first.
	s.SetOraclePrice(s.MarketIndex, 50_000, 50_000)
}

// TestCrossingFillRoundTrip drives the full trading round-trip:
//
//  1. user0/user1 each deposit 100k USDC into the perp account.
//  2. user0 rests an ASK @ 50000 x 100, user1 crosses with a BID — the
//     matching engine fills both and produces equal-and-opposite positions.
//  3. user0 closes their -100 short by placing a BID @ 50000 x 100 (no
//     resting ask, so it rests); user1 then sells 100 BACK as an ASK
//     which crosses, returning both to flat.
//  4. With OI back to zero, both users should be able to withdraw the
//     bulk of their starting collateral. We verify by withdrawing 90k.
func (s *TradingFlowSuite) TestCrossingFillRoundTrip() {
	const depositUSDC = uint64(100_000_000_000) // 100k USDC, 6-decimal external
	const orderQty = uint64(100)
	const orderPrice = uint32(50_000)

	// 1. Bring both makers funded with collateral.
	s.DepositUSDC(&s.Users[0], depositUSDC)
	s.DepositUSDC(&s.Users[1], depositUSDC)

	pre0 := s.QueryCollateral(s.Users[0].AccountIndex)
	pre1 := s.QueryCollateral(s.Users[1].AccountIndex)
	s.Require().True(pre0.IsPositive(), "user0 must hold positive collateral after deposit")
	s.Require().True(pre1.IsPositive(), "user1 must hold positive collateral after deposit")

	// 2. user0 rests an ask, then user1's bid crosses it.
	askResp := s.PlaceLimitOrder(s.Users[0], msg.OrderOpts{
		MarketIndex:      s.MarketIndex,
		IsAsk:            true,
		Price:            orderPrice,
		BaseAmount:       orderQty,
		ClientOrderIndex: 1,
	})
	s.Require().Equal(perptypes.OrderStatusOpen, askResp.Status, "ask should rest on the book first")

	bid, ask := s.QueryBestBidAsk(s.MarketIndex)
	s.Require().Equal(uint32(0), bid)
	s.Require().Equal(orderPrice, ask, "best ask must reflect user0's resting order")

	bidResp := s.PlaceLimitOrder(s.Users[1], msg.OrderOpts{
		MarketIndex:      s.MarketIndex,
		IsAsk:            false,
		Price:            orderPrice,
		BaseAmount:       orderQty,
		ClientOrderIndex: 2,
	})
	s.Require().Equal(perptypes.OrderStatusFilled, bidResp.Status, "bid must fully fill against the resting ask")
	s.Require().Equal(orderQty, bidResp.FilledBaseAmount)

	// Positions are signed: user0 (maker, ask) ends up short; user1 long.
	pos0 := s.QueryPositionSize(s.Users[0].AccountIndex, s.MarketIndex)
	pos1 := s.QueryPositionSize(s.Users[1].AccountIndex, s.MarketIndex)
	s.Require().Equal(math.NewInt(-int64(orderQty)), pos0, "user0 must hold a short position equal to the trade size")
	s.Require().Equal(math.NewInt(int64(orderQty)), pos1, "user1 must hold an equal long position")

	// Open interest must reflect the size of the fill.
	details := s.QueryMarketDetails(s.MarketIndex)
	s.Require().Equal(int64(orderQty), details.OpenInterest)

	// Treasury account_index=0 should now hold the combined fees taken
	// from taker + maker (taker_fee=10bps, maker_fee=5bps).
	treasury := s.QueryCollateral(perptypes.TreasuryAccountIndex)
	s.Require().True(treasury.IsPositive(), "treasury must accrue maker + taker fees")

	// Each maker / taker should have collateral strictly less than what
	// they deposited (post-fee).
	post0Open := s.QueryCollateral(s.Users[0].AccountIndex)
	post1Open := s.QueryCollateral(s.Users[1].AccountIndex)
	s.Require().True(post0Open.LT(pre0), "user0 collateral should drop by maker fee")
	s.Require().True(post1Open.LT(pre1), "user1 collateral should drop by taker fee")

	// 3. Close out: user0 rests a bid, user1 crosses with an ask. Both
	//    return to a flat position and OI returns to 0.
	closeBidResp := s.PlaceLimitOrder(s.Users[0], msg.OrderOpts{
		MarketIndex:      s.MarketIndex,
		IsAsk:            false,
		Price:            orderPrice,
		BaseAmount:       orderQty,
		ClientOrderIndex: 3,
	})
	s.Require().Equal(perptypes.OrderStatusOpen, closeBidResp.Status)

	closeAskResp := s.PlaceLimitOrder(s.Users[1], msg.OrderOpts{
		MarketIndex:      s.MarketIndex,
		IsAsk:            true,
		Price:            orderPrice,
		BaseAmount:       orderQty,
		ClientOrderIndex: 4,
	})
	s.Require().Equal(perptypes.OrderStatusFilled, closeAskResp.Status)

	pos0Final := s.QueryPositionSize(s.Users[0].AccountIndex, s.MarketIndex)
	pos1Final := s.QueryPositionSize(s.Users[1].AccountIndex, s.MarketIndex)
	s.Require().True(pos0Final.IsZero(), "user0 position must be flat after the close trade")
	s.Require().True(pos1Final.IsZero(), "user1 position must be flat after the close trade")

	// Open interest must track the current net outstanding base across
	// accounts (sum(|position|)/2). After a full round-trip (open then
	// close) both sides are flat, so OI returns to zero.
	finalDetails := s.QueryMarketDetails(s.MarketIndex)
	s.Require().Equal(int64(0), finalDetails.OpenInterest,
		"open interest must return to zero after a full round-trip")

	// Best-bid/ask must drain back to (0, 0) since every order was filled.
	bidEnd, askEnd := s.QueryBestBidAsk(s.MarketIndex)
	s.Require().Equal(uint32(0), bidEnd)
	s.Require().Equal(uint32(0), askEnd)

	// 4. Withdraw most of the collateral. Both users should still be
	//    flat enough that the risk-check passes.
	const withdrawUSDC = uint64(90_000_000_000) // 90k USDC
	s.WithdrawUSDC(s.Users[0], withdrawUSDC)
	s.WithdrawUSDC(s.Users[1], withdrawUSDC)
}

// TestStaleMarkPriceBlocksRiskChange exercises the wiring of the
// median-mark staleness gate end-to-end. The gate lives on
// `x/market.Keeper.gateMarkPrice` (driven by
// `market.Params.MaxMarkPriceStalenessMs`), and every downstream consumer
// (x/trade.Engine.Apply via IsValidRiskChangeFrom → ComputeCrossRisk
// → MarketKeeper.GetMarkPriceAndDetails, x/matching trigger activation
// also via MarketKeeper.GetMarkPriceAndDetails, x/liquidation ADL ranking)
// MUST observe the same gate. Concretely:
//
//  1. Seed a fresh mark + open a small position so the risk pipeline
//     is exercised on a non-empty book.
//  2. Manually expire `LastMarkPriceRefreshTimestamp` on MarketDetails — this
//     mimics the funding BeginBlocker falling silent for longer than
//     `MaxMarkPriceStalenessMs`.
//  3. Attempt another order. The trade engine routes through
//     `IsValidRiskChangeFrom → ComputeCrossRisk →
//     MarketKeeper.GetMarkPriceAndDetails`, which must fail-closed with
//     `markettypes.ErrStaleMarkPrice`.
//
// Retained as a regression guard against an earlier wiring bug where
// the gate lived on x/risk with a late-bound funding keeper: the risk
// keeper was copied by value into x/trade / x/liquidation / x/matching
// BEFORE the funding keeper was injected, so consumers silently
// bypassed the gate. Owning the gate on market keeper eliminates the
// late-binding hazard entirely (no setter, no mutable field), but the
// e2e assertion still pins the end-to-end fail-closed contract.
func (s *TradingFlowSuite) TestStaleMarkPriceBlocksRiskChange() {
	const depositUSDC = uint64(100_000_000_000)
	const orderQty = uint64(100)
	const orderPrice = uint32(50_000)

	s.DepositUSDC(&s.Users[0], depositUSDC)
	s.DepositUSDC(&s.Users[1], depositUSDC)

	// Open a small cross position so the risk pipeline has something
	// to classify on the next change attempt.
	askResp := s.PlaceLimitOrder(s.Users[0], msg.OrderOpts{
		MarketIndex:      s.MarketIndex,
		IsAsk:            true,
		Price:            orderPrice,
		BaseAmount:       orderQty,
		ClientOrderIndex: 1,
	})
	s.Require().Equal(perptypes.OrderStatusOpen, askResp.Status)
	bidResp := s.PlaceLimitOrder(s.Users[1], msg.OrderOpts{
		MarketIndex:      s.MarketIndex,
		IsAsk:            false,
		Price:            orderPrice,
		BaseAmount:       orderQty,
		ClientOrderIndex: 2,
	})
	s.Require().Equal(perptypes.OrderStatusFilled, bidResp.Status)

	// Manually expire LastMarkPriceRefreshTimestamp so the staleness gate
	// trips. We deliberately bypass the funding BeginBlocker (which
	// would otherwise refresh it every block) to isolate the gate.
	d, err := s.App.MarketKeeper.GetMarketDetails(s.Ctx, s.MarketIndex)
	s.Require().NoError(err)
	d.LastMarkPriceRefreshTimestamp = 0
	s.Require().NoError(s.App.MarketKeeper.SetMarketDetails(s.Ctx, d))

	// Any further user-initiated order on this market MUST be rejected
	// because the trade engine consults the risk keeper, which now
	// fails-closed on the stale mark. PlaceLimitOrderExpectErr asserts
	// the matching msg server returns a non-nil error so the test
	// fails immediately if the consumers ever go back to bypassing
	// the gate.
	_, err = msg.PlaceLimitOrder(s.App, s.Ctx, s.Users[0], msg.OrderOpts{
		MarketIndex:      s.MarketIndex,
		IsAsk:            false,
		Price:            orderPrice,
		BaseAmount:       orderQty,
		ClientOrderIndex: 3,
	})
	s.Require().Error(err,
		"stale mark must propagate to x/trade via the market staleness gate (MarketKeeper.GetMarkPriceAndDetails); if this passes the consumer call sites are bypassing the gate")
}
