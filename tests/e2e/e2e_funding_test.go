package e2e_test

import (
	"testing"
	"time"

	"cosmossdk.io/math"

	perptypes "github.com/perpdex/perpdex-l1/types"

	"github.com/perpdex/perpdex-l1/tests/e2e"
	"github.com/perpdex/perpdex-l1/tests/e2e/msg"
)

// FundingSuite drives the perpetual-funding pipeline (Lighter spec):
//
//   - x/funding BeginBlocker samples the Lighter premium
//     `(max(0, ImpactBid - index) - max(0, index - ImpactAsk)) / index`
//     once a minute per market and accumulates the sample into
//     `MarketDetails.AggregatePremiumSum`.
//   - When `now - LastFundingRoundTimestamp >= FundingPeriodMs` (one hour),
//     the keeper averages the samples, applies the double clamp -- the
//     small clamp adjusts towards the configured InterestRate -- and divides
//     by `FundingPeriodDivisor` (default 8). The 1-hour rate is then folded
//     into `MarketDetails.FundingRatePrefixSum` as `mark_price * rate` so
//     positions can settle via `pos * delta_prefix / TICK`.
//   - On the next position touch (e.g. a trade or explicit
//     `SettlePositionFunding`) the position absorbs the prefix-sum delta
//     into its EntryQuote, matching the Lighter formula
//     `funding = position * mark * fundingRate`.
type FundingSuite struct {
	e2e.PerpdexTestSuite

	BTCAssetIndex uint32
	MarketIndex   uint32
}

func TestFundingSuite(t *testing.T) {
	e2e.RunSuite(t, new(FundingSuite))
}

func (s *FundingSuite) SetupTest() {
	// Need 4 users: two position holders plus one for resting orders that
	// drive impact_bid/impact_ask, plus one as oracle provider.
	s.NumUsers = 4
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
	s.MarketIndex = s.CreatePerpMarket(msg.DefaultPerpMarketOpts(1, s.BTCAssetIndex))
}

// TestPremiumAccumulatesAndSettles walks the Lighter funding pipeline
// end-to-end:
//
//  1. user0/user1 each deposit 100k USDC and open opposing positions of
//     1_000_000 base @ 50_000 -- a non-trivial size so the per-round funding
//     rate yields a measurable EntryQuote delta.
//  2. user2 places a resting bid @ 49_999, user3 a resting ask @ 50_001
//     with enough depth to absorb the full `ImpactUsdcAmount` -- so the
//     funding sampler reads ImpactBid=49_999, ImpactAsk=50_001.
//  3. user3 is whitelisted as the oracle provider; they inject
//     index_price=49_500, mark_price=50_000. Premium per-sample is
//     `(max(0, 49_999 - 49_500) - 0) * 1e6 / 49_500 = 10_080`.
//  4. We advance one full funding period (1 hour + a minute of slack) so
//     BeginBlocker takes a fresh sample then settles the round.
//
// Expected per-round math:
//
//		premium = 10_080 (single sample average)
//		correction = clamp(ir - premium, -SmallClamp, +SmallClamp)
//		           = clamp(0 - 10_080, -500, +500) = -500
//		smallClamped = 10_080 + (-500) = 9_580
//		bigClamped   = clamp(9_580, -40_000, +40_000) = 9_580
//		rate         = 9_580 / 8 = 1_197 (truncated)
//		prefix delta = mark * rate = 50_000 * 1_197 = 59_850_000
//
//	 5. Force an explicit SettlePositionFunding for user0's short. The
//	    payment is `pos * delta / TICK = -1_000_000 * 59_850_000 / 1_000_000
//	    = -59_850_000`, so EntryQuote drops by 59_850_000 (short pays funding
//	    when premium is positive).
func (s *FundingSuite) TestPremiumAccumulatesAndSettles() {
	const depositUSDC = uint64(100_000_000_000) // 100k USDC, external precision
	const orderQty = uint64(1_000_000)
	const tradePrice = uint32(50_000)
	const restingBidPrice = uint32(49_999)
	const restingAskPrice = uint32(50_001)
	const restingQty = uint64(12_000) // 12_000 * 50_000 = 6e8 quote-ticks ≥ ImpactUsdcAmount=5e8

	for i := 0; i < 4; i++ {
		s.DepositUSDC(&s.Users[i], depositUSDC)
	}

	// Seed the oracle up front so the risk keeper can classify the
	// fresh positions created by the crossing fill below (audit fix:
	// missing prices on non-zero positions now fail closed).
	s.SetOraclePrice(s.MarketIndex, tradePrice, tradePrice)

	// 1. Open opposite positions: user0 short 1M @ 50000, user1 long 1M.
	askResp := s.PlaceLimitOrder(s.Users[0], msg.OrderOpts{
		MarketIndex:      s.MarketIndex,
		IsAsk:            true,
		Price:            tradePrice,
		BaseAmount:       orderQty,
		ClientOrderIndex: 1,
	})
	s.Require().Equal(perptypes.OrderStatusOpen, askResp.Status)

	bidResp := s.PlaceLimitOrder(s.Users[1], msg.OrderOpts{
		MarketIndex:      s.MarketIndex,
		IsAsk:            false,
		Price:            tradePrice,
		BaseAmount:       orderQty,
		ClientOrderIndex: 2,
	})
	s.Require().Equal(perptypes.OrderStatusFilled, bidResp.Status)

	// Sanity check: positions are opposite and equal-sized.
	user0Pos := s.QueryPositionSize(s.Users[0].AccountIndex, s.MarketIndex)
	user1Pos := s.QueryPositionSize(s.Users[1].AccountIndex, s.MarketIndex)
	s.Require().Equal(math.NewInt(-int64(orderQty)), user0Pos)
	s.Require().Equal(math.NewInt(int64(orderQty)), user1Pos)

	// 2. Lay down impact-defining resting orders that won't cross. The
	// resting depth must cover the orderbook's `ImpactUsdcAmount`
	// notional on each side or the funding sampler skips the sample.
	_ = s.PlaceLimitOrder(s.Users[2], msg.OrderOpts{
		MarketIndex:      s.MarketIndex,
		IsAsk:            false,
		Price:            restingBidPrice,
		BaseAmount:       restingQty,
		ClientOrderIndex: 3,
	})
	_ = s.PlaceLimitOrder(s.Users[3], msg.OrderOpts{
		MarketIndex:      s.MarketIndex,
		IsAsk:            true,
		Price:            restingAskPrice,
		BaseAmount:       restingQty,
		ClientOrderIndex: 4,
	})

	// 3. Override the mark/index with the values the funding sampler
	// should actually see. The whitelist and seed price were already
	// applied before the fills above.
	const indexPrice = uint32(49_500)
	const markPrice = uint32(50_000)
	s.SetOraclePrice(s.MarketIndex, indexPrice, markPrice)
	// This deterministic suite bypasses the live vote-extension oracle, so
	// allow the seeded fixture to remain fresh across the one-hour funding
	// advance below. Production gets a fresh PreBlock oracle write each block.
	oracleParams, err := s.App.OracleKeeper.Params.Get(s.Ctx)
	s.Require().NoError(err)
	oracleParams.MaxAgeMs = perptypes.FundingPeriod + time.Minute.Milliseconds()
	s.Require().NoError(s.App.OracleKeeper.Params.Set(s.Ctx, oracleParams))

	// Pre-condition checks that pin down the inputs the funding sampler
	// will see during the next BeginBlocker.
	bid, ask := s.QueryBestBidAsk(s.MarketIndex)
	s.Require().Equal(restingBidPrice, bid, "resting bid must populate the book before sampling")
	s.Require().Equal(restingAskPrice, ask, "resting ask must populate the book before sampling")
	oraclePrice := s.QueryOraclePrice(s.MarketIndex)
	s.Require().Equal(indexPrice, oraclePrice.IndexPrice)

	// Capture the funding prefix sum before any block has had a chance to
	// settle.
	preDetails := s.QueryMarketDetails(s.MarketIndex)
	prePrefix := preDetails.FundingRatePrefixSum

	// 4. Advance one full funding period plus a minute of slack so the
	// next BeginBlocker:
	//   - clears the per-market 1-minute throttle (LastUpdatedTimestamp
	//     was last bumped during DepositUSDC / PlaceLimitOrder blocks),
	//     allowing one fresh premium sample;
	//   - crosses the hour-boundary
	//     `now - LastFundingRoundTimestamp >= FundingPeriodMs` and so
	//     fires the settle branch.
	s.AdvanceBlockBy(time.Duration(perptypes.FundingPeriod)*time.Millisecond + time.Minute)

	postDetails := s.QueryMarketDetails(s.MarketIndex)
	postPrefix := postDetails.FundingRatePrefixSum

	// premium per sample = (49_999 - 49_500) * 1e6 / 49_500 = 10_080
	// correction = clamp(0 - 10_080, -500, +500) = -500
	// smallClamped = 10_080 - 500 = 9_580
	// bigClamped = 9_580 (within ±40_000)
	// rate = 9_580 / 8 = 1_197
	// prefix delta = mark * rate = 50_000 * 1_197 = 59_850_000
	const expectedDelta = int64(59_850_000)
	delta := postPrefix.Sub(prePrefix)
	s.Require().Equal(expectedDelta, delta.Int64(),
		"prefix sum must advance by mark * rate = 50_000 * 1_197 = 59_850_000")

	// 5. Force an explicit funding settlement on user0's short position.
	preEQUser0 := s.QueryPosition(s.Users[0].AccountIndex, s.MarketIndex).EntryQuote
	s.Require().NoError(
		s.App.FundingKeeper.SettlePositionFunding(s.Ctx, s.Users[0].AccountIndex, s.MarketIndex),
	)
	postEQUser0 := s.QueryPosition(s.Users[0].AccountIndex, s.MarketIndex).EntryQuote

	// pay = position * delta / TICK
	//     = -1_000_000 * 59_850_000 / 1_000_000
	//     = -59_850_000
	// SettlePositionFunding adds `pay` to entry_quote: short positions
	// pay funding when premium is positive (longs receive), which
	// manifests as EntryQuote getting more negative for the short side
	// and uPnL = pos*mark - EntryQuote dropping by exactly the funding
	// paid.
	const expectedPay = int64(-59_850_000)
	moved := postEQUser0.Sub(preEQUser0)
	s.Require().Equal(expectedPay, moved.Int64(),
		"short position EntryQuote must drop by pos * delta / TICK = -59_850_000")
}
