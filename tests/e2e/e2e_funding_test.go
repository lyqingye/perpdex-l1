package e2e_test

import (
	"testing"
	"time"

	"cosmossdk.io/math"

	perptypes "github.com/perpdex/perpdex-l1/types"

	"github.com/perpdex/perpdex-l1/tests/e2e"
	"github.com/perpdex/perpdex-l1/tests/e2e/msg"
)

// FundingSuite drives the perpetual-funding pipeline:
//
//   - x/funding BeginBlocker samples (impact_price - index_price) every
//     block once an oracle price is set, accumulating it into
//     `MarketDetails.AggregatePremiumSum`.
//   - When `now - LastFundingRoundTimestamp >= FundingPeriodMs / Divisor`
//     (~7.5min by default) the keeper averages the samples, double-clamps
//     the result and bumps `MarketDetails.FundingRatePrefixSum`.
//   - On the next position touch (e.g. trade or explicit
//     SettlePositionFunding) the position absorbs the prefix-sum delta
//     into its EntryQuote.
//
// We exercise the first settle in a single block (since
// LastFundingRoundTimestamp starts at 0 the OR-branch fires immediately on
// the first sample) and then verify a position picks up the funding rate
// when SettlePositionFunding is called.
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

// TestPremiumAccumulatesAndSettles walks the funding pipeline end-to-end:
//
//  1. user0/user1 each deposit 100k USDC and open opposing positions of
//     1_000_000 base @ 50000 — a non-trivial size so a small funding rate
//     yields a measurable EntryQuote delta.
//  2. user2 places resting bid @ 49999, user3 places resting ask @ 50001
//     so impact_bid / impact_ask is well-defined for the funding sampler.
//  3. user3 is whitelisted as the oracle provider; they inject
//     index_price=49500, mark_price=50000. Premium = ~+10101 (well above
//     FundingSmallClamp=500), so the rate post-clamp pegs at +500.
//  4. The first AdvanceBlock fires BeginBlocker which samples the premium
//     and immediately settles (because LastFundingRoundTimestamp==0). The
//     prefix sum should grow by the clamped rate.
//  5. We then call FundingKeeper.SettlePositionFunding on user0's short
//     position and assert EntryQuote moved by `position * delta / TICK`.
func (s *FundingSuite) TestPremiumAccumulatesAndSettles() {
	const depositUSDC = uint64(100_000_000_000) // 100k USDC, external precision
	const orderQty = uint64(1_000_000)
	const tradePrice = uint32(50_000)
	const restingBidPrice = uint32(49_999)
	const restingAskPrice = uint32(50_001)
	const restingQty = uint64(100)
	const restingDepositUSDC = uint64(50_000_000_000) // 50k USDC

	for i := 0; i < 4; i++ {
		s.DepositUSDC(&s.Users[i], depositUSDC)
		_ = restingDepositUSDC // bookkeeping note for reviewers
	}

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

	// 2. Lay down impact-defining resting orders that won't cross.
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

	// 3. Whitelist user3 as oracle provider then inject (index, mark).
	s.AddOracleProvider(s.Users[3].Address, "test-funding-provider")
	const indexPrice = uint32(49_500)
	const markPrice = uint32(50_000)
	s.InjectPrice(s.Users[3].Address, s.MarketIndex, indexPrice, markPrice)

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

	// 4. AdvanceBlock — the first block where BeginBlocker observes both
	//    impact and oracle prices. The base suite's earlier AdvanceBlock
	//    set `Metadata.LastFundingRoundTimestamp` to the current block's
	//    time, so we must wait at least one settle interval
	//    (FundingPeriodMs / FundingPeriodDivisor = 7.5 min) before the
	//    funding round will be closed. We advance 8 minutes to be safe.
	s.AdvanceBlockBy(8 * time.Minute)

	postDetails := s.QueryMarketDetails(s.MarketIndex)
	postPrefix := postDetails.FundingRatePrefixSum

	// premium = (impact - index) * tick / index = (50000-49500)*1e6/49500
	//        ≈ +10101  → above FundingSmallClamp=500 → clamped to +500.
	// After settle: aggregate_premium_sum reset, samples reset, prefix
	// advanced by +500 (so the new prefix is greater than the pre-prefix).
	delta := postPrefix.Sub(prePrefix)
	s.Require().True(delta.IsPositive(),
		"funding rate prefix sum must advance once a positive premium settles (delta=%s)", delta.String())
	s.Require().LessOrEqual(delta.Int64(), int64(perptypes.FundingSmallClamp),
		"clamped rate must not exceed FundingSmallClamp (delta=%s)", delta.String())

	// 5. Force an explicit funding settlement on user0's short position.
	preEQUser0 := s.QueryPosition(s.Users[0].AccountIndex, s.MarketIndex).EntryQuote
	s.Require().NoError(
		s.App.FundingKeeper.SettlePositionFunding(s.Ctx, s.Users[0].AccountIndex, s.MarketIndex),
	)
	postEQUser0 := s.QueryPosition(s.Users[0].AccountIndex, s.MarketIndex).EntryQuote

	// pay = position * delta / TICK = -1_000_000 * 500 / 1_000_000 = -500.
	// SettlePositionFunding adds `pay` to entry_quote: short positions
	// pay funding when premium is positive (longs receive), which
	// manifests as EntryQuote getting more negative for the short side.
	moved := postEQUser0.Sub(preEQUser0)
	s.Require().True(moved.IsNegative(),
		"short position must lose entry-quote on positive funding (moved=%s)", moved.String())
}
