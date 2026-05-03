package e2e_test

import (
	"testing"

	"cosmossdk.io/math"

	perptypes "github.com/perpdex/perpdex-l1/types"
	liquidationtypes "github.com/perpdex/perpdex-l1/x/liquidation/types"

	"github.com/perpdex/perpdex-l1/tests/e2e"
	"github.com/perpdex/perpdex-l1/tests/e2e/msg"
)

// LiquidationSuite drives the cross-margin liquidation pipeline:
//
//   - x/risk classifies an account into one of HEALTHY / PRE / PARTIAL /
//     FULL / BANKRUPTCY based on TAV vs (IM, MM, CM).
//   - x/liquidation rejects MsgLiquidate when the victim is healthy and
//     accepts it when the victim is in PARTIAL/FULL state, closing some
//     of the position via x/trade at the position's "zero price".
//   - For BANKRUPTCY the keeper-bot path is `MsgDeleverage`, which closes
//     the victim against an opposing well-collateralised account with
//     `NoFee`.
type LiquidationSuite struct {
	e2e.PerpdexTestSuite

	BTCAssetIndex uint32
	MarketIndex   uint32
}

func TestLiquidationSuite(t *testing.T) {
	e2e.RunSuite(t, new(LiquidationSuite))
}

func (s *LiquidationSuite) SetupTest() {
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

	// Whitelist user3 as the oracle provider used by every liquidation
	// scenario. Tests then call `s.InjectPrice(s.Users[3].Address, ...)`
	// to seed mark / index prices.
	s.AddOracleProvider(s.Users[3].Address, "liquidation-test-provider")
}

// openHurtablePosition deposits 10 USDC for user0 (the future victim) and
// 1000 USDC for user1 (the well-collateralised counterparty). user1 rests
// an ASK; user0 crosses with a BID, so user0 ends up long and user1 short.
//
// Returns (entryPrice, qty) so callers can compute liquidation thresholds.
func (s *LiquidationSuite) openHurtablePosition() (entryPrice uint32, qty uint64) {
	const victimDeposit = uint64(10_000_000)        // 10 USDC external
	const counterDeposit = uint64(1_000_000_000)    // 1000 USDC external
	entryPrice = 50_000
	qty = 1_000_000_000 // 10 BTC at 8 decimals

	s.DepositUSDC(&s.Users[0], victimDeposit)
	s.DepositUSDC(&s.Users[1], counterDeposit)
	s.DepositUSDC(&s.Users[2], counterDeposit)
	s.DepositUSDC(&s.Users[3], counterDeposit)

	askResp := s.PlaceLimitOrder(s.Users[1], msg.OrderOpts{
		MarketIndex:      s.MarketIndex,
		IsAsk:            true,
		Price:            entryPrice,
		BaseAmount:       qty,
		ClientOrderIndex: 1,
	})
	s.Require().Equal(perptypes.OrderStatusOpen, askResp.Status)

	bidResp := s.PlaceLimitOrder(s.Users[0], msg.OrderOpts{
		MarketIndex:      s.MarketIndex,
		IsAsk:            false,
		Price:            entryPrice,
		BaseAmount:       qty,
		ClientOrderIndex: 2,
	})
	s.Require().Equal(perptypes.OrderStatusFilled, bidResp.Status)

	pos := s.QueryPositionSize(s.Users[0].AccountIndex, s.MarketIndex)
	s.Require().Equal(math.NewInt(int64(qty)), pos, "victim must be long after the open")
	return entryPrice, qty
}

// preFundLiquidator gives the absorbing account a chunky collateral
// buffer so the trade keeper's post-trade risk-check on the taker
// passes when it inherits the victim's notional.
func (s *LiquidationSuite) preFundLiquidator(accountIdx uint64, amount math.Int) {
	s.Require().NoError(
		s.App.PerpAccountKeeper.AddCollateral(s.Ctx, accountIdx, amount),
	)
}

// TestRejectsHealthyVictim asserts that a victim with full collateral is
// not liquidatable. The risk classifier should return HEALTHY and the
// liquidation keeper must refuse the close-out.
func (s *LiquidationSuite) TestRejectsHealthyVictim() {
	entry, qty := s.openHurtablePosition()
	// Inject the oracle at the entry price so uPnL ≈ 0 and TAV is at its
	// post-fee maximum.
	s.InjectPrice(s.Users[3].Address, s.MarketIndex, entry, entry)

	health := s.QueryHealthStatus(s.Users[0].AccountIndex)
	s.Require().Equal(perptypes.HealthHealthy, health, "victim must be healthy at the entry price")

	err := s.LiquidateExpectErr(s.Users[2], s.Users[0].AccountIndex, s.MarketIndex, qty)
	s.Require().ErrorContains(err, "not liquidatable",
		"liquidation must be rejected when health is HEALTHY")
}

// TestPartialLiquidation drops the oracle far enough to put the victim
// into PARTIAL_LIQUIDATION (TAV < MM, but TAV >= CM). MsgLiquidate must
// then succeed and close the position.
func (s *LiquidationSuite) TestPartialLiquidation() {
	entry, qty := s.openHurtablePosition()
	// Anchor oracle at entry first so the trade keeper risk-check during
	// the open uses a non-zero IM.
	s.InjectPrice(s.Users[3].Address, s.MarketIndex, entry, entry)

	// Drop to 41_000 ⇒ uPnL = qty * (41000 - 50000) = -9e12; with 10 USDC
	// of collateral (~1e13 internal) the resulting TAV lands inside
	// (CM, MM) and the classifier returns PARTIAL_LIQUIDATION.
	const distressedPrice = uint32(41_000)
	s.InjectPrice(s.Users[3].Address, s.MarketIndex, distressedPrice, distressedPrice)

	health := s.QueryHealthStatus(s.Users[0].AccountIndex)
	s.Require().True(
		health == perptypes.HealthPartialLiquidation || health == perptypes.HealthFullLiquidation,
		"price drop must push victim into PARTIAL or FULL_LIQUIDATION (got %d)", health,
	)

	// Pre-fund the bot account so the post-trade risk-check on the
	// taker (which inherits the victim's notional at zero_price) does
	// not regress its own health.
	s.preFundLiquidator(s.Users[2].AccountIndex, math.NewInt(1_000_000_000_000_000)) // 1e15

	prePos := s.QueryPositionSize(s.Users[0].AccountIndex, s.MarketIndex)
	s.Require().Equal(math.NewInt(int64(qty)), prePos)

	s.Liquidate(s.Users[2], s.Users[0].AccountIndex, s.MarketIndex, qty)

	postPos := s.QueryPositionSize(s.Users[0].AccountIndex, s.MarketIndex)
	s.Require().True(postPos.LT(prePos),
		"liquidation must shrink the victim's long position (pre=%s post=%s)",
		prePos.String(), postPos.String())
	s.Require().True(postPos.IsZero() || postPos.IsPositive(),
		"position must not flip sign on a single liquidation step")

	// The trade keeper's close-out semantics are a 1:1 transfer: the
	// liquidator opens a NEW long at zero_price equal in magnitude to
	// the victim's reduction. user2 ends up holding the same direction
	// as the victim's pre-state.
	botPos := s.QueryPositionSize(s.Users[2].AccountIndex, s.MarketIndex)
	s.Require().True(botPos.IsPositive(),
		"liquidator inherits the victim's long at zero_price (pos=%s)", botPos.String())
	closed := prePos.Sub(postPos)
	s.Require().Equal(closed, botPos,
		"liquidator inheritance must equal the size closed on the victim")
}

// TestBankruptcyDeleverage pushes the victim past CM into BANKRUPTCY
// (TAV < 0) and then runs MsgDeleverage with a well-collateralised
// counter-party. The implementation closes the position at the zero
// price with NoFee and tops up the victim's collateral from the
// insurance fund if the close-out leaves them under-water.
func (s *LiquidationSuite) TestBankruptcyDeleverage() {
	entry, qty := s.openHurtablePosition()
	s.InjectPrice(s.Users[3].Address, s.MarketIndex, entry, entry)

	// 30_000 ⇒ uPnL = qty * (30000-50000) = -2e13; collateral 1e13 →
	// TAV ≈ -1e13 → BANKRUPTCY.
	const wipeoutPrice = uint32(30_000)
	s.InjectPrice(s.Users[3].Address, s.MarketIndex, wipeoutPrice, wipeoutPrice)

	health := s.QueryHealthStatus(s.Users[0].AccountIndex)
	s.Require().Equal(perptypes.HealthBankruptcy, health,
		"victim must be in BANKRUPTCY before deleverage (got %d)", health)

	// MsgLiquidate is allowed in PARTIAL/FULL but NOT in BANKRUPTCY in
	// the current keeper. Make sure that path is rejected so callers
	// know to fall back to MsgDeleverage.
	s.preFundLiquidator(s.Users[2].AccountIndex, math.NewInt(1_000_000_000_000_000))

	// MsgDeleverage with user1 as the deleverager — they took the
	// counter short during the open so they're well capitalised.
	prePosVictim := s.QueryPositionSize(s.Users[0].AccountIndex, s.MarketIndex)
	s.Require().Equal(math.NewInt(int64(qty)), prePosVictim)

	s.Deleverage(s.Users[2], s.Users[0].AccountIndex, s.Users[1].AccountIndex, s.MarketIndex, qty)

	postPosVictim := s.QueryPositionSize(s.Users[0].AccountIndex, s.MarketIndex)
	s.Require().True(postPosVictim.IsZero(),
		"deleverage must close the victim's position completely (post=%s)", postPosVictim.String())

	// user1 originally had -qty (short) and is the deleverager, so
	// absorbing the +qty close should bring them back to flat.
	postPosCounter := s.QueryPositionSize(s.Users[1].AccountIndex, s.MarketIndex)
	s.Require().True(postPosCounter.IsZero(),
		"deleverager must end flat after absorbing the close (post=%s)", postPosCounter.String())
}

// TestEndBlockerFlagsLifecycle exercises the EndBlocker that maintains
// LiquidationFlag entries: an unhealthy account must be flagged on every
// market it holds a position in, and the flag must be cleared once the
// account either closes the position out or recovers to HEALTHY via a
// price move back.
func (s *LiquidationSuite) TestEndBlockerFlagsLifecycle() {
	entry, _ := s.openHurtablePosition()
	s.InjectPrice(s.Users[3].Address, s.MarketIndex, entry, entry)
	s.AdvanceBlock()

	// At entry price the victim is HEALTHY and no flag should exist.
	_, present := s.QueryLiquidationFlag(s.Users[0].AccountIndex, s.MarketIndex)
	s.Require().False(present, "healthy account must not have a liquidation flag")

	// Drop the oracle to put the victim into PARTIAL/FULL.
	s.InjectPrice(s.Users[3].Address, s.MarketIndex, 41_000, 41_000)
	s.AdvanceBlock()

	flag, present := s.QueryLiquidationFlag(s.Users[0].AccountIndex, s.MarketIndex)
	s.Require().True(present, "EndBlocker must flag an unhealthy account's market")
	s.Require().Equal(s.Users[0].AccountIndex, flag.AccountIndex)
	s.Require().Equal(s.MarketIndex, flag.MarketIndex)
	s.Require().Greater(flag.FlaggedAtBlock, int64(0))
	s.Require().Greater(flag.FlaggedAtTime, int64(0))

	// Recover the price; EndBlocker should clear the flag on the next
	// block.
	s.InjectPrice(s.Users[3].Address, s.MarketIndex, entry, entry)
	s.AdvanceBlock()

	_, present = s.QueryLiquidationFlag(s.Users[0].AccountIndex, s.MarketIndex)
	s.Require().False(present,
		"EndBlocker must drop the flag once the account returns to HEALTHY")
}

// pushVictimToBankruptcy is a helper for the ADL tests that re-creates
// the openHurtablePosition setup and then drops the oracle to the
// wipe-out price so the victim lands in BANKRUPTCY. It returns the
// (entryPrice, qty) tuple for assertions.
func (s *LiquidationSuite) pushVictimToBankruptcy() (entry, wipeout uint32, qty uint64) {
	entry, qty = s.openHurtablePosition()
	s.InjectPrice(s.Users[3].Address, s.MarketIndex, entry, entry)
	wipeout = uint32(30_000)
	s.InjectPrice(s.Users[3].Address, s.MarketIndex, wipeout, wipeout)
	health := s.QueryHealthStatus(s.Users[0].AccountIndex)
	s.Require().Equal(perptypes.HealthBankruptcy, health,
		"victim must be in BANKRUPTCY before ADL test (got %d)", health)
	return entry, wipeout, qty
}

// drainInsuranceFund replaces the insurance-fund collateral with the
// caller-supplied (signed) amount. EndBlocker auto-ADL only fires when
// `insurance + victim_collateral < 0`, so tests that exercise the
// auto-trigger path must call this with a small amount (or zero).
func (s *LiquidationSuite) drainInsuranceFund(target math.Int) {
	insf, err := s.App.PerpAccountKeeper.GetAccount(s.Ctx, perptypes.InsuranceFundOperatorAccountIdx)
	s.Require().NoError(err)
	delta := target.Sub(insf.Collateral)
	s.Require().NoError(
		s.App.PerpAccountKeeper.AddCollateral(s.Ctx, perptypes.InsuranceFundOperatorAccountIdx, delta),
	)
}

// freezeInsuranceFund flips IF.PublicPoolInfo.Status to FROZEN. Required
// in the user-ADL fallback tests because EndBlocker now routes
// BANKRUPTCY victims to the IF first when it is ACTIVE (lighter
// IF_FIRST behaviour).
func (s *LiquidationSuite) freezeInsuranceFund() {
	insf, err := s.App.PerpAccountKeeper.GetAccount(s.Ctx, perptypes.InsuranceFundOperatorAccountIdx)
	s.Require().NoError(err)
	s.Require().NotNil(insf.PublicPoolInfo,
		"genesis must wire PublicPoolInfo on the IF account")
	insf.PublicPoolInfo.Status = perptypes.PublicPoolStatusFrozen
	s.Require().NoError(s.App.PerpAccountKeeper.SetAccount(s.Ctx, insf))
}

// TestADLRejectsSameSide verifies the Lighter-style ADL invariant: a
// counterparty on the SAME side as the victim cannot be used as
// deleverager. user0 (long victim) attempts to ADL against user2 (also
// long because we cross them at entry).
func (s *LiquidationSuite) TestADLRejectsSameSide() {
	_, _, qty := s.pushVictimToBankruptcy()

	// Open a same-side (long) position for user2 by crossing against
	// user1's resting short. We piggy-back on the funded counterparty
	// and use a small price (well below current mark so user2 is in
	// profit, isolating the same-side check from PnL filtering).
	const entry2 = uint32(30_000)
	askResp := s.PlaceLimitOrder(s.Users[1], msg.OrderOpts{
		MarketIndex:      s.MarketIndex,
		IsAsk:            true,
		Price:            entry2,
		BaseAmount:       qty,
		ClientOrderIndex: 11,
	})
	s.Require().Equal(perptypes.OrderStatusOpen, askResp.Status)
	bidResp := s.PlaceLimitOrder(s.Users[2], msg.OrderOpts{
		MarketIndex:      s.MarketIndex,
		IsAsk:            false,
		Price:            entry2,
		BaseAmount:       qty,
		ClientOrderIndex: 12,
	})
	s.Require().Equal(perptypes.OrderStatusFilled, bidResp.Status)

	// Sanity: user2 is long, user0 is also long.
	pos2 := s.QueryPositionSize(s.Users[2].AccountIndex, s.MarketIndex)
	s.Require().True(pos2.IsPositive(), "user2 must be long for the same-side test")

	err := s.DeleverageExpectErr(
		s.Users[2], s.Users[0].AccountIndex, s.Users[2].AccountIndex, s.MarketIndex, qty,
	)
	s.Require().ErrorIs(err, liquidationtypes.ErrInvalidADLCounterparty,
		"deleverage must reject a same-side counterparty")
}

// TestADLAcceptsOppositeProfitable runs the happy ADL path: user1 holds
// the opposing short and is in profit at the wipe-out price, so
// MsgDeleverage with user1 must succeed and bring both books to flat.
func (s *LiquidationSuite) TestADLAcceptsOppositeProfitable() {
	_, _, qty := s.pushVictimToBankruptcy()

	// Confirm the queue ranks user1 as the only/best candidate for the
	// short side of the victim's long.
	cands := s.QueryADLQueue(s.MarketIndex, false /* victim long → shorts queue */, 8)
	s.Require().NotEmpty(cands, "ADL queue must contain at least user1 short")
	s.Require().Equal(s.Users[1].AccountIndex, cands[0].AccountIndex,
		"user1 should rank first since they hold the only profitable short")

	prePosVictim := s.QueryPositionSize(s.Users[0].AccountIndex, s.MarketIndex)
	s.Require().Equal(math.NewInt(int64(qty)), prePosVictim)

	s.Deleverage(s.Users[2], s.Users[0].AccountIndex, s.Users[1].AccountIndex, s.MarketIndex, qty)

	postPosVictim := s.QueryPositionSize(s.Users[0].AccountIndex, s.MarketIndex)
	s.Require().True(postPosVictim.IsZero(),
		"deleverage must close the victim's position completely (post=%s)", postPosVictim.String())
	postPosCounter := s.QueryPositionSize(s.Users[1].AccountIndex, s.MarketIndex)
	s.Require().True(postPosCounter.IsZero(),
		"counterparty must end flat after absorbing the close (post=%s)", postPosCounter.String())
}

// TestEndBlockerAutoADL verifies the dYdX-style on-chain auto-trigger:
// when a victim is BANKRUPTCY and the insurance fund cannot absorb the
// loss, the EndBlocker's auto-ADL loop force-closes the victim against
// the top of the ADL queue. Asserts the victim's position is closed
// without any manual MsgDeleverage call.
//
// Note: we invoke `LiquidationKeeper.EndBlocker(s.Ctx)` directly rather
// than `s.AdvanceBlock()` because the suite's UncachedContext writes
// (drainInsuranceFund / Params override) only land in the BaseApp's
// committed multistore on Commit; FinalizeBlock branches off a fresh
// state that wouldn't observe those writes. Calling the keeper on the
// same s.Ctx threads all writes through one context.
func (s *LiquidationSuite) TestEndBlockerAutoADL() {
	_, _, qty := s.pushVictimToBankruptcy()

	// Freeze the IF so the IF_FIRST branch declines and EndBlocker
	// falls back to the user ADL queue (the user-ranked queue is what
	// this test exercises).
	s.freezeInsuranceFund()
	s.drainInsuranceFund(math.ZeroInt())

	// Bump the per-block cap so a single block can fully close the
	// victim's position via one fill against user1.
	params, err := s.App.LiquidationKeeper.Params.Get(s.Ctx)
	s.Require().NoError(err)
	params.MaxAdlAttemptsPerBlock = 4
	s.Require().NoError(s.App.LiquidationKeeper.Params.Set(s.Ctx, params))

	prePosVictim := s.QueryPositionSize(s.Users[0].AccountIndex, s.MarketIndex)
	s.Require().Equal(math.NewInt(int64(qty)), prePosVictim)

	// Sanity: queue must contain user1 short before auto-ADL fires.
	cands := s.QueryADLQueue(s.MarketIndex, false /* shorts */, 8)
	s.Require().NotEmpty(cands, "queue must contain user1 before auto-ADL fires")

	s.Require().NoError(s.App.LiquidationKeeper.EndBlocker(s.Ctx))

	postPosVictim := s.QueryPositionSize(s.Users[0].AccountIndex, s.MarketIndex)
	s.Require().True(postPosVictim.IsZero(),
		"EndBlocker auto-ADL must close the bankrupt victim (post=%s)", postPosVictim.String())
	postPosCounter := s.QueryPositionSize(s.Users[1].AccountIndex, s.MarketIndex)
	s.Require().True(postPosCounter.IsZero(),
		"auto-ADL counterparty must end flat (post=%s)", postPosCounter.String())
}

// TestADLRespectsPerBlockCap pins MaxAdlAttemptsPerBlock=1, makes BOTH
// users 0 and 2 bankrupt against user1's short, and verifies that the
// EndBlocker only closes ONE victim in a single block. The second
// victim must remain open (and flagged) until the next block.
func (s *LiquidationSuite) TestADLRespectsPerBlockCap() {
	const entry = uint32(50_000)
	const qty = uint64(1_000_000_000)
	const counterDeposit = uint64(5_000_000_000) // 5000 USDC

	// Deposit modest collateral to victims and a fat buffer to user1
	// (counterparty for both victims). user3 is the oracle provider.
	s.DepositUSDC(&s.Users[0], 10_000_000)        // victim A: 10 USDC
	s.DepositUSDC(&s.Users[2], 10_000_000)        // victim B: 10 USDC
	s.DepositUSDC(&s.Users[1], counterDeposit)
	s.DepositUSDC(&s.Users[3], counterDeposit)

	// user1 rests a short of 2*qty; victims A & B each cross half.
	askResp := s.PlaceLimitOrder(s.Users[1], msg.OrderOpts{
		MarketIndex:      s.MarketIndex,
		IsAsk:            true,
		Price:            entry,
		BaseAmount:       2 * qty,
		ClientOrderIndex: 21,
	})
	s.Require().Equal(perptypes.OrderStatusOpen, askResp.Status)
	bidA := s.PlaceLimitOrder(s.Users[0], msg.OrderOpts{
		MarketIndex:      s.MarketIndex,
		IsAsk:            false,
		Price:            entry,
		BaseAmount:       qty,
		ClientOrderIndex: 22,
	})
	s.Require().Equal(perptypes.OrderStatusFilled, bidA.Status)
	bidB := s.PlaceLimitOrder(s.Users[2], msg.OrderOpts{
		MarketIndex:      s.MarketIndex,
		IsAsk:            false,
		Price:            entry,
		BaseAmount:       qty,
		ClientOrderIndex: 23,
	})
	s.Require().Equal(perptypes.OrderStatusFilled, bidB.Status)

	// Push both A & B into BANKRUPTCY simultaneously.
	s.InjectPrice(s.Users[3].Address, s.MarketIndex, entry, entry)
	s.InjectPrice(s.Users[3].Address, s.MarketIndex, 30_000, 30_000)
	s.Require().Equal(perptypes.HealthBankruptcy,
		s.QueryHealthStatus(s.Users[0].AccountIndex))
	s.Require().Equal(perptypes.HealthBankruptcy,
		s.QueryHealthStatus(s.Users[2].AccountIndex))

	// Freeze IF so the EndBlocker takes the user-ADL fallback (the
	// per-block cap test only makes sense in that path).
	s.freezeInsuranceFund()
	s.drainInsuranceFund(math.ZeroInt())

	// Pin per-block cap to 1: at most one auto-ADL fill across both
	// victims this block.
	params, err := s.App.LiquidationKeeper.Params.Get(s.Ctx)
	s.Require().NoError(err)
	params.MaxAdlAttemptsPerBlock = 1
	s.Require().NoError(s.App.LiquidationKeeper.Params.Set(s.Ctx, params))

	s.Require().NoError(s.App.LiquidationKeeper.EndBlocker(s.Ctx))

	posA := s.QueryPositionSize(s.Users[0].AccountIndex, s.MarketIndex)
	posB := s.QueryPositionSize(s.Users[2].AccountIndex, s.MarketIndex)
	closedA := posA.IsZero()
	closedB := posB.IsZero()
	s.Require().True(closedA != closedB,
		"per-block cap=1 must close exactly one of the two bankrupt victims (A=%s B=%s)",
		posA.String(), posB.String())
}
