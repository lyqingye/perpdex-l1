package e2e_test

import (
	"testing"

	"cosmossdk.io/math"

	perptypes "github.com/perpdex/perpdex-l1/types"
	accountkeepertest "github.com/perpdex/perpdex-l1/x/account/keeper/keepertest"
	accounttypes "github.com/perpdex/perpdex-l1/x/account/types"
	matchingtypes "github.com/perpdex/perpdex-l1/x/matching/types"

	"github.com/perpdex/perpdex-l1/tests/e2e"
	"github.com/perpdex/perpdex-l1/tests/e2e/msg"
)

// PublicPoolSuite covers the PUBLIC_POOL pipeline:
// Create / Update / Mint / Burn / StrategyTransfer / ForceBurn,
// plus the cross-cutting `pool/IF cannot place orders` and
// EndBlocker IF_FIRST routing rules.
type PublicPoolSuite struct {
	e2e.PerpdexTestSuite

	BTCAssetIndex uint32
	MarketIndex   uint32
}

func TestPublicPoolSuite(t *testing.T) {
	e2e.RunSuite(t, new(PublicPoolSuite))
}

func (s *PublicPoolSuite) SetupTest() {
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
	// Risk fails closed on any non-zero position without a mark price;
	// seed a default so crossing orders in the IF-first scenario do
	// not fail before we explicitly push the price down.
	s.SetOraclePrice(s.MarketIndex, 50_000, 50_000)
}

// ---------- helpers ----------

// fundAndPool funds user `idx` with `bankUSDC` external USDC, deposits
// it as collateral, and creates a PUBLIC_POOL with `initialShares`
// shares + `feeBps` operator fee. Returns (poolIndex, masterIndex).
func (s *PublicPoolSuite) fundAndPool(
	idx int, bankUSDC, initialShares uint64, feeBps, minRate uint32,
) (poolIdx, masterIdx uint64) {
	s.DepositUSDC(&s.Users[idx], bankUSDC)
	masterIdx = s.Users[idx].AccountIndex
	poolIdx = s.CreatePublicPool(s.Users[idx], masterIdx, initialShares, feeBps, minRate)
	return poolIdx, masterIdx
}

// ---------- 1) TestCreatePublicPool ----------

func (s *PublicPoolSuite) TestCreatePublicPool() {
	const initialShares = uint64(1000)
	const feeBps = uint32(0)
	const minRate = uint32(0)
	// 1000 shares * INITIAL_POOL_SHARE_VALUE(1000) = 1_000_000 uusdc =
	// 1 USDC external. Deposit 10 USDC so master has plenty.
	poolIdx, _ := s.fundAndPool(0, 10_000_000, initialShares, feeBps, minRate)

	a := s.QueryAccount(poolIdx)
	s.Require().Equal(perptypes.PublicPoolAccountType, a.AccountType)
	s.Require().Equal(perptypes.AccountTradingModeSimple, a.AccountTradingMode)

	info, ok := s.QueryPublicPoolInfo(poolIdx)
	s.Require().True(ok)
	s.Require().Equal(perptypes.PublicPoolStatusActive, info.Status)
	s.Require().Equal(math.NewIntFromUint64(initialShares), info.TotalShares)
	s.Require().Equal(math.NewIntFromUint64(initialShares), info.OperatorShares)
	s.Require().Len(info.Strategies, perptypes.NbStrategies)

	// Master collateral should have decreased by the seed amount.
	master := s.QueryAccount(s.Users[0].AccountIndex)
	expected := math.NewIntFromUint64(10_000_000).
		Sub(math.NewIntFromUint64(initialShares).Mul(math.NewIntFromUint64(perptypes.InitialPoolShareValue))).
		Mul(math.NewIntFromUint64(perptypes.USDCToCollateralMultiplier))
	s.Require().Equal(expected.String(), master.Collateral.String())
}

// ---------- 2) TestPoolCannotPlaceOrder ----------

func (s *PublicPoolSuite) TestPoolCannotPlaceOrder() {
	poolIdx, _ := s.fundAndPool(0, 10_000_000, 1000, 0, 0)

	// Sender owns the pool's master, so IsAuthorized passes; the gate
	// is the AccountType pool/IF check inside CreateOrder.
	_, err := msg.PlaceLimitOrderRaw(s.App, s.Ctx, msg.OrderOpts{
		Sender:           s.Users[0].Address.String(),
		AccountIndex:     poolIdx,
		MarketIndex:      s.MarketIndex,
		IsAsk:            true,
		Price:            50_000,
		BaseAmount:       1_000_000,
		ClientOrderIndex: 1,
	})
	s.Require().ErrorIs(err, matchingtypes.ErrPoolCannotPlaceOrder)
}

// ---------- 3) TestMintShares_OperatorRate ----------

func (s *PublicPoolSuite) TestMintShares_OperatorRate() {
	// operator floor = ShareTick → operator must hold 100% of shares.
	// Initial 1000 shares all owned by the operator. A non-operator
	// mint of any size should violate the floor and be rejected.
	poolIdx, _ := s.fundAndPool(0, 10_000_000, 1000, 0, uint32(perptypes.ShareTick))

	// Fund user1 and try to mint a tiny share: rejected.
	s.DepositUSDC(&s.Users[1], 10_000_000)
	err := s.MintSharesExpectErr(s.Users[1], poolIdx, 1_000_000)
	s.Require().ErrorIs(err, accounttypes.ErrOperatorRateViolation)
}

// ---------- 4) TestBurnShares_Profit_OperatorFee ----------

func (s *PublicPoolSuite) TestBurnShares_Profit_OperatorFee() {
	// Operator fee = 50_000 (half of FeeTick=1_000_000 ⇒ 5%). No
	// operator-rate floor.
	const feeBps = uint32(50_000)
	poolIdx, _ := s.fundAndPool(0, 10_000_000, 1000, feeBps, 0)

	// Non-operator (user1) mints 9 USDC worth of shares.
	s.DepositUSDC(&s.Users[1], 10_000_000)
	shares := s.MintShares(s.Users[1], poolIdx, 9_000_000)
	s.Require().True(shares.IsPositive())

	infoBefore, _ := s.QueryPublicPoolInfo(poolIdx)
	opSharesBefore := infoBefore.OperatorShares

	// Inject artificial profit: the pool is currently flat (no
	// positions); we bump its collateral directly to simulate trading
	// gains. NAV doubles ⇒ 9 USDC principal becomes ~18 USDC value.
	pool := s.QueryAccount(poolIdx)
	doubled := pool.Collateral
	s.Require().NoError(s.App.PerpAccountKeeper.AddCollateral(s.Ctx, poolIdx, doubled))

	// Burn all of user1's shares.
	pre := s.QueryCollateral(s.Users[1].AccountIndex)
	usdcRedeemed := s.BurnShares(s.Users[1], poolIdx, shares)
	post := s.QueryCollateral(s.Users[1].AccountIndex)
	s.Require().Greater(usdcRedeemed, uint64(0))
	s.Require().True(post.Sub(pre).IsPositive(),
		"user1 master must receive uusdc on burn (delta=%s)", post.Sub(pre).String())

	infoAfter, _ := s.QueryPublicPoolInfo(poolIdx)
	s.Require().True(infoAfter.OperatorShares.GT(opSharesBefore),
		"operator must accrue fee shares from the realized profit (before=%s after=%s)",
		opSharesBefore.String(), infoAfter.OperatorShares.String())
}

// ---------- 5) TestBurnShares_Cooldown ----------

func (s *PublicPoolSuite) TestBurnShares_Cooldown() {
	// LLP cooldown only applies on Params.LiquidityPoolIndex; default is
	// the IF account. Override Params to point at a fresh user pool, so
	// the cooldown gate is exercised on the regular flow.
	poolIdx, _ := s.fundAndPool(0, 10_000_000, 1000, 0, 0)

	params, err := s.App.PerpAccountKeeper.Params.Get(s.Ctx)
	s.Require().NoError(err)
	params.LiquidityPoolIndex = poolIdx
	params.LiquidityPoolCooldownPeriodMs = int64(60 * 1000) // 60s
	s.Require().NoError(s.App.PerpAccountKeeper.Params.Set(s.Ctx, params))

	s.DepositUSDC(&s.Users[1], 10_000_000)
	shares := s.MintShares(s.Users[1], poolIdx, 5_000_000)

	// Immediate burn must fail with cooldown.
	err = s.BurnSharesExpectErr(s.Users[1], poolIdx, shares)
	s.Require().ErrorIs(err, accounttypes.ErrCooldownNotElapsed)

	// Advance past cooldown; burn now succeeds.
	s.AdvanceBlockBy(120 * 1_000_000_000) // 120s in ns
	usdc := s.BurnShares(s.Users[1], poolIdx, shares)
	s.Require().Greater(usdc, uint64(0))
}

// ---------- 6) TestUpdatePublicPool_Freeze ----------

func (s *PublicPoolSuite) TestUpdatePublicPool_Freeze() {
	poolIdx, _ := s.fundAndPool(0, 10_000_000, 1000, 0, 0)

	// Operator successfully freezes a clean pool.
	s.UpdatePublicPool(s.Users[0].Address.String(), poolIdx,
		perptypes.PublicPoolStatusFrozen, 0, 0)
	info, _ := s.QueryPublicPoolInfo(poolIdx)
	s.Require().Equal(perptypes.PublicPoolStatusFrozen, info.Status)

	// Frozen pools cannot be updated again (Update requires ACTIVE).
	err := s.UpdatePublicPoolExpectErr(s.Users[0].Address.String(), poolIdx,
		perptypes.PublicPoolStatusActive, 0, 0)
	s.Require().ErrorIs(err, accounttypes.ErrPoolNotActive)
}

// ---------- 7) TestStrategyTransfer_OnlyIF ----------

func (s *PublicPoolSuite) TestStrategyTransfer_OnlyIF() {
	poolIdx, _ := s.fundAndPool(0, 10_000_000, 1000, 0, 0)

	// Regular pool: strategy transfer must be rejected.
	err := s.StrategyTransferExpectErr(s.Users[0].Address.String(), poolIdx, 0, 1, math.OneInt())
	s.Require().ErrorIs(err, accounttypes.ErrNotInsuranceFund)

	// IF (genesis account 1): authority signs, transfer succeeds. We
	// first add some collateral so strategy[0] has 100 to move.
	s.Require().NoError(s.App.PerpAccountKeeper.AddCollateral(
		s.Ctx, perptypes.InsuranceFundOperatorAccountIdx, math.NewInt(1_000_000)))
	insf, err := s.App.PerpAccountKeeper.GetAccount(s.Ctx, perptypes.InsuranceFundOperatorAccountIdx)
	s.Require().NoError(err)
	insf.PublicPoolInfo.Strategies[0] = math.NewInt(100)
	s.Require().NoError(accountkeepertest.SetAccountForTest(s.Ctx, s.App.PerpAccountKeeper, insf))

	s.StrategyTransfer(s.GovAddress.String(),
		perptypes.InsuranceFundOperatorAccountIdx, 0, 1, math.NewInt(40))

	insf, err = s.App.PerpAccountKeeper.GetAccount(s.Ctx, perptypes.InsuranceFundOperatorAccountIdx)
	s.Require().NoError(err)
	s.Require().Equal("60", insf.PublicPoolInfo.Strategies[0].String())
	s.Require().Equal("40", insf.PublicPoolInfo.Strategies[1].String())
}

// ---------- 8) TestForceBurnShares ----------

func (s *PublicPoolSuite) TestForceBurnShares() {
	// Mint into the IF (canonical LLP) so we have a depositor to burn.
	// Bypass MsgMintShares for simplicity by directly crediting state:
	// the ForceBurn handler doesn't care HOW the shares got there.
	insfIdx := perptypes.InsuranceFundOperatorAccountIdx

	s.DepositUSDC(&s.Users[1], 10_000_000)
	master, err := s.App.PerpAccountKeeper.GetAccount(s.Ctx, s.Users[1].AccountIndex)
	s.Require().NoError(err)
	master.PublicPoolShares = []accounttypes.PublicPoolShare{{
		PublicPoolIndex: insfIdx,
		ShareAmount:     math.NewInt(500),
		PrincipalAmount: math.NewInt(500_000),
		EntryTimestamp:  s.BlockTime().UnixMilli(),
	}}
	s.Require().NoError(accountkeepertest.SetAccountForTest(s.Ctx, s.App.PerpAccountKeeper, master))

	insf, err := s.App.PerpAccountKeeper.GetAccount(s.Ctx, insfIdx)
	s.Require().NoError(err)
	insf.PublicPoolInfo.TotalShares = math.NewInt(1500)
	insf.PublicPoolInfo.OperatorShares = math.NewInt(1000)
	s.Require().NoError(accountkeepertest.SetAccountForTest(s.Ctx, s.App.PerpAccountKeeper, insf))
	s.Require().NoError(s.App.PerpAccountKeeper.AddCollateral(
		s.Ctx, insfIdx, math.NewInt(1_500_000_000)))

	// Force-burn 100 shares immediately (would be rejected by the
	// regular Burn path due to cooldown).
	usdc := s.ForceBurnShares(insfIdx, s.Users[1].AccountIndex, math.NewInt(100))
	s.Require().Greater(usdc, uint64(0))

	master, err = s.App.PerpAccountKeeper.GetAccount(s.Ctx, s.Users[1].AccountIndex)
	s.Require().NoError(err)
	s.Require().Equal("400", master.PublicPoolShares[0].ShareAmount.String())
}

// ---------- 9) TestEndBlockerIFFirst ----------

func (s *PublicPoolSuite) TestEndBlockerIFFirst() {
	// Open a hurtable position similar to LiquidationSuite but inline
	// here so we don't depend on that suite's wiring.
	const entry = uint32(50_000)
	const qty = uint64(1_000_000_000)
	s.DepositUSDC(&s.Users[0], 10_000_000)
	s.DepositUSDC(&s.Users[1], 1_000_000_000)
	s.PlaceLimitOrder(s.Users[1], msg.OrderOpts{
		MarketIndex: s.MarketIndex, IsAsk: true,
		Price: entry, BaseAmount: qty, ClientOrderIndex: 1,
	})
	s.PlaceLimitOrder(s.Users[0], msg.OrderOpts{
		MarketIndex: s.MarketIndex, IsAsk: false,
		Price: entry, BaseAmount: qty, ClientOrderIndex: 2,
	})
	// Push to BANKRUPTCY.
	s.SetOraclePrice(s.MarketIndex, entry, entry)
	s.SetOraclePrice(s.MarketIndex, 30_000, 30_000)
	s.Require().Equal(perptypes.HealthBankruptcy,
		s.QueryHealthStatus(s.Users[0].AccountIndex))

	// Pre-fund IF with a chunky buffer (NoRiskCheck=true on the
	// absorb fill, but the IF still needs collateral to sit on the
	// resulting position notional).
	s.Require().NoError(s.App.PerpAccountKeeper.AddCollateral(
		s.Ctx, perptypes.InsuranceFundOperatorAccountIdx,
		math.NewInt(1_000_000_000_000_000)))

	// Bump per-block cap so the absorb can complete in one block.
	params, err := s.App.LiquidationKeeper.Params.Get(s.Ctx)
	s.Require().NoError(err)
	params.MaxAdlAttemptsPerBlock = 4
	s.Require().NoError(s.App.LiquidationKeeper.Params.Set(s.Ctx, params))

	s.Require().NoError(s.App.LiquidationKeeper.EndBlocker(s.Ctx))

	// Victim is closed, IF holds the residual long.
	postVictim := s.QueryPositionSize(s.Users[0].AccountIndex, s.MarketIndex)
	s.Require().True(postVictim.IsZero(), "IF_FIRST must close the bankrupt victim")
	postIF := s.QueryPositionSize(perptypes.InsuranceFundOperatorAccountIdx, s.MarketIndex)
	s.Require().True(postIF.IsPositive(),
		"IF must inherit the victim's long via NoRiskCheck Deleverage (got %s)", postIF.String())
	postCounter := s.QueryPositionSize(s.Users[1].AccountIndex, s.MarketIndex)
	s.Require().True(postCounter.IsNegative(),
		"user1 short must be untouched in IF_FIRST routing (got %s)", postCounter.String())
}
