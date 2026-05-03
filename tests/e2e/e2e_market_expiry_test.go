package e2e_test

import (
	"testing"
	"time"

	"cosmossdk.io/math"

	perptypes "github.com/perpdex/perpdex-l1/types"

	"github.com/perpdex/perpdex-l1/tests/e2e"
	"github.com/perpdex/perpdex-l1/tests/e2e/msg"
)

// MarketExpirySuite covers the auto-expire path that x/market's EndBlocker
// implements: when `Market.ExpiryTimestamp > 0 && now >= expiry` the
// keeper flips the market into MarketStatusExpired and asks
// `liquidationKeeper.ApplyExitPosition` to close out residual positions
// against the insurance fund at the last mark price (with NoFee +
// NoRiskCheck so the insurance fund can absorb residual size).
type MarketExpirySuite struct {
	e2e.PerpdexTestSuite

	BTCAssetIndex uint32
	MarketIndex   uint32
	Expiry        int64
}

func TestMarketExpirySuite(t *testing.T) {
	e2e.RunSuite(t, new(MarketExpirySuite))
}

func (s *MarketExpirySuite) SetupTest() {
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

	// Pick an expiry 5 minutes after the suite's notional clock. We
	// advance well past this in the test body to fire the EndBlocker.
	opts := msg.DefaultPerpMarketOpts(1, s.BTCAssetIndex)
	s.Expiry = s.BlockTime().Add(5 * time.Minute).UnixMilli()
	opts.ExpiryTimestamp = s.Expiry
	s.MarketIndex = s.CreatePerpMarket(opts)
}

// TestMarketAutoExpiresOnSchedule walks the expiry contract:
//
//  1. After SetupTest the market is ACTIVE and ExpiryTimestamp is set to
//     "now + 5 minutes".
//  2. After advancing one block well past the expiry, the EndBlocker
//     should flip the status to EXPIRED.
//  3. Subsequent MsgCreateOrder calls must be rejected with the
//     "market not active" error.
func (s *MarketExpirySuite) TestMarketAutoExpiresOnSchedule() {
	mkt := s.QueryMarket(s.MarketIndex)
	s.Require().Equal(perptypes.MarketStatusActive, mkt.Status, "market must start active")
	s.Require().Equal(s.Expiry, mkt.ExpiryTimestamp,
		"expiry timestamp must round-trip through CreateMarket")

	// Open some collateral so we can later attempt to place an order
	// against the expired market and assert it gets rejected.
	const depositUSDC = uint64(50_000_000_000) // 50k USDC
	s.DepositUSDC(&s.Users[0], depositUSDC)

	// Advance the clock 6 minutes — comfortably past the 5-minute expiry
	// — in a single block so the market EndBlocker fires once with
	// `now >= ExpiryTimestamp`.
	s.AdvanceBlockBy(6 * time.Minute)

	postMkt := s.QueryMarket(s.MarketIndex)
	s.Require().Equal(perptypes.MarketStatusExpired, postMkt.Status,
		"EndBlocker must mark the market expired once block time crosses ExpiryTimestamp")

	// Any further trade attempts must be rejected. We bypass the suite
	// shim (which fails on error) and use the raw msg helper.
	_, err := msg.PlaceLimitOrder(s.App, s.Ctx, s.Users[0], msg.OrderOpts{
		MarketIndex:      s.MarketIndex,
		IsAsk:            true,
		Price:            50_000,
		BaseAmount:       1_000,
		ClientOrderIndex: 1,
	})
	s.Require().Error(err, "expired market must reject new orders")
	s.Require().ErrorContains(err, "market not active",
		"the matching keeper must surface the canonical 'market not active' error")
}

// TestMarketStillActiveBeforeExpiry sanity-checks the negative case: a
// block that lands a millisecond shy of the expiry must NOT flip status.
func (s *MarketExpirySuite) TestMarketStillActiveBeforeExpiry() {
	// Advance by 4 minutes — strictly before the 5-minute expiry.
	s.AdvanceBlockBy(4 * time.Minute)
	mkt := s.QueryMarket(s.MarketIndex)
	s.Require().Equal(perptypes.MarketStatusActive, mkt.Status,
		"market must remain active until block time crosses the expiry")
}

// TestExpiryClosesOpenPositions exercises the full exit-position path:
// open a long/short pair, let the funding BeginBlocker latch the mark
// price into MarketDetails, advance past expiry and assert that
// ApplyExitPosition zeroes out every non-insurance-fund position in the
// expired market against `InsuranceFundOperatorAccountIdx`.
func (s *MarketExpirySuite) TestExpiryClosesOpenPositions() {
	// Need an oracle provider so the funding BeginBlocker can latch
	// MarkPrice into MarketDetails.
	s.AddOracleProvider(s.Users[3].Address, "expiry-test-provider")

	// Two well-funded users so they can open opposite sides.
	const deposit = uint64(1_000_000_000_000) // 1e6 USDC external
	s.DepositUSDC(&s.Users[0], deposit)
	s.DepositUSDC(&s.Users[1], deposit)

	// Cross at 50_000 for 1 BTC.
	const entry = uint32(50_000)
	// Seed the oracle before the first fill so the risk keeper can
	// classify the resulting positions.
	s.InjectPrice(s.Users[3].Address, s.MarketIndex, entry, entry)
	const qty = uint64(100_000_000) // 1 BTC at 8 decimals
	askResp := s.PlaceLimitOrder(s.Users[1], msg.OrderOpts{
		MarketIndex:      s.MarketIndex,
		IsAsk:            true,
		Price:            entry,
		BaseAmount:       qty,
		ClientOrderIndex: 1,
	})
	s.Require().Equal(perptypes.OrderStatusOpen, askResp.Status)
	bidResp := s.PlaceLimitOrder(s.Users[0], msg.OrderOpts{
		MarketIndex:      s.MarketIndex,
		IsAsk:            false,
		Price:            entry,
		BaseAmount:       qty,
		ClientOrderIndex: 2,
	})
	s.Require().Equal(perptypes.OrderStatusFilled, bidResp.Status)

	// Inject and advance one block so x/funding latches the mark price
	// into MarketDetails before x/market expires the market.
	s.InjectPrice(s.Users[3].Address, s.MarketIndex, entry, entry)
	s.AdvanceBlock()

	md := s.QueryMarketDetails(s.MarketIndex)
	s.Require().Equal(entry, md.MarkPrice,
		"funding BeginBlocker must latch mark price before expiry")

	// Sanity: positions exist in opposite directions before expiry.
	s.Require().True(s.QueryPositionSize(s.Users[0].AccountIndex, s.MarketIndex).IsPositive())
	s.Require().True(s.QueryPositionSize(s.Users[1].AccountIndex, s.MarketIndex).IsNegative())

	// Pre-fund the insurance fund a bit so it has SOME collateral on
	// hand. ApplyExitPosition uses NoRiskCheck=true so this is not
	// strictly required, but it is closer to mainnet conditions.
	s.Require().NoError(
		s.App.PerpAccountKeeper.AddCollateral(
			s.Ctx, perptypes.InsuranceFundOperatorAccountIdx, math.NewInt(1_000_000_000_000),
		),
	)

	// Advance past expiry.
	s.AdvanceBlockBy(6 * time.Minute)

	mkt := s.QueryMarket(s.MarketIndex)
	s.Require().Equal(perptypes.MarketStatusExpired, mkt.Status)

	// Both user positions must be closed.
	s.Require().True(s.QueryPositionSize(s.Users[0].AccountIndex, s.MarketIndex).IsZero(),
		"long must be exit-closed once the market expires")
	s.Require().True(s.QueryPositionSize(s.Users[1].AccountIndex, s.MarketIndex).IsZero(),
		"short must be exit-closed once the market expires")
}
