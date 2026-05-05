package e2e

import (
	"cosmossdk.io/math"

	perptypes "github.com/perpdex/perpdex-l1/types"
	accounttypes "github.com/perpdex/perpdex-l1/x/account/types"
	assettypes "github.com/perpdex/perpdex-l1/x/asset/types"
	liquidationtypes "github.com/perpdex/perpdex-l1/x/liquidation/types"
	markettypes "github.com/perpdex/perpdex-l1/x/market/types"
	matchingtypes "github.com/perpdex/perpdex-l1/x/matching/types"
	oracletypes "github.com/perpdex/perpdex-l1/x/oracle/types"

	"github.com/perpdex/perpdex-l1/tests/e2e/msg"
	"github.com/perpdex/perpdex-l1/tests/e2e/query"
)

// This file defines the high-level shim methods on PerpdexTestSuite. Each
// shim wraps a function from the `msg` or `query` sub-packages, threads in
// the suite's running app + ctx + governance address, and uses the
// embedded suite.Require() to fail fast on errors. They keep test bodies
// compact: instead of `s.Require().NoError(msg.RegisterAsset(s.App, s.Ctx,
// s.GovAddress, opts))` callers write `s.RegisterAsset(opts)`.

// ---------- asset / market shims ----------

// RegisterAsset registers a new asset via gov authority and returns the
// allocated asset_index. Fails the test on error.
func (s *PerpdexTestSuite) RegisterAsset(opts msg.AssetOpts) uint32 {
	resp, err := msg.RegisterAsset(s.App, s.Ctx, s.GovAddress, opts)
	s.Require().NoError(err)
	return resp.AssetIndex
}

// CreatePerpMarket spins up a perpetual market and returns its index.
func (s *PerpdexTestSuite) CreatePerpMarket(opts msg.MarketOpts) uint32 {
	if opts.MarketType == 0 {
		opts.MarketType = perptypes.MarketTypePerps
	}
	_, err := msg.CreateMarket(s.App, s.Ctx, s.GovAddress, opts)
	s.Require().NoError(err)
	return opts.MarketIndex
}

// UpdateMarket toggles status / expiry on an existing market.
func (s *PerpdexTestSuite) UpdateMarket(marketIndex uint32, newStatus uint32, expiryTimestamp int64) {
	_, err := msg.UpdateMarket(s.App, s.Ctx, s.GovAddress, marketIndex, newStatus, expiryTimestamp)
	s.Require().NoError(err)
}

// ---------- account shims ----------

// DepositUSDC deposits USDC from the user's bank balance into the perp
// collateral route. The user's AccountIndex is populated on first call.
func (s *PerpdexTestSuite) DepositUSDC(user *TestUser, amount uint64) {
	_, err := msg.DepositUSDC(s.App, s.Ctx, user, amount)
	s.Require().NoError(err)
}

// Deposit is the generic variant: caller chooses asset_index + route.
func (s *PerpdexTestSuite) Deposit(user *TestUser, assetIndex uint32, route uint32, amount uint64) {
	_, err := msg.Deposit(s.App, s.Ctx, user, assetIndex, route, amount)
	s.Require().NoError(err)
}

// Withdraw burns collateral / spot back to the user's bank address.
func (s *PerpdexTestSuite) Withdraw(user TestUser, assetIndex, route uint32, amount uint64) {
	_, err := msg.Withdraw(s.App, s.Ctx, user, assetIndex, route, amount)
	s.Require().NoError(err)
}

// WithdrawUSDC is a convenience wrapper around Withdraw for the canonical
// USDC perp route.
func (s *PerpdexTestSuite) WithdrawUSDC(user TestUser, amount uint64) {
	s.Withdraw(user, perptypes.USDCAssetIndex, perptypes.RouteTypePerps, amount)
}

// ---------- matching shims ----------

// PlaceLimitOrder dispatches MsgCreateOrder for a GTT limit order. Returns
// (orderIndex, status, filledBaseAmount).
func (s *PerpdexTestSuite) PlaceLimitOrder(user TestUser, opts msg.OrderOpts) *matchingtypes.MsgCreateOrderResponse {
	resp, err := msg.PlaceLimitOrder(s.App, s.Ctx, user, opts)
	s.Require().NoError(err)
	return resp
}

// PlaceMarketOrder dispatches MsgCreateOrder for a IOC market order.
func (s *PerpdexTestSuite) PlaceMarketOrder(
	user TestUser, marketIndex uint32, isAsk bool, baseAmount, clientOrderIndex uint64,
) *matchingtypes.MsgCreateOrderResponse {
	resp, err := msg.PlaceMarketOrder(s.App, s.Ctx, user, marketIndex, isAsk, baseAmount, clientOrderIndex)
	s.Require().NoError(err)
	return resp
}

// CancelOrder removes a single resting order.
func (s *PerpdexTestSuite) CancelOrder(user TestUser, marketIndex uint32, orderIndex uint64) {
	_, err := msg.CancelOrder(s.App, s.Ctx, user, marketIndex, orderIndex)
	s.Require().NoError(err)
}

// ---------- oracle shims ----------

// SetOraclePrice writes a (mark, index) price tuple straight into the
// oracle keeper, bypassing the vote-extension pipeline. Used by every
// e2e test that needs a deterministic price fixture: in production the
// equivalent path is the proposer-injected MsgAggregateOracleVotes
// emitted by oracle.VoteExtensionHandler.PrepareProposal.
func (s *PerpdexTestSuite) SetOraclePrice(marketIndex uint32, indexPrice, markPrice uint32) {
	now := s.Ctx.BlockTime().UnixMilli()
	height := s.Ctx.BlockHeight()
	err := s.App.OracleKeeper.SetPrice(s.Ctx, oracletypes.OraclePrice{
		MarketIndex:          marketIndex,
		IndexPrice:           indexPrice,
		MarkPrice:            markPrice,
		LastUpdatedTimestamp: now,
		LastUpdatedHeight:    height,
	})
	s.Require().NoError(err)
}

// ---------- liquidation shims ----------

// Liquidate is the keeper-bot entry point.
func (s *PerpdexTestSuite) Liquidate(
	bot TestUser, victim uint64, marketIndex uint32, baseAmount uint64,
) {
	_, err := msg.Liquidate(s.App, s.Ctx, bot, victim, marketIndex, baseAmount)
	s.Require().NoError(err)
}

// LiquidateExpectErr asserts MsgLiquidate fails with a non-nil error;
// helpful when verifying that healthy accounts cannot be touched.
func (s *PerpdexTestSuite) LiquidateExpectErr(
	bot TestUser, victim uint64, marketIndex uint32, baseAmount uint64,
) error {
	_, err := msg.Liquidate(s.App, s.Ctx, bot, victim, marketIndex, baseAmount)
	s.Require().Error(err)
	return err
}

// Deleverage runs the BANKRUPTCY-state escape hatch.
func (s *PerpdexTestSuite) Deleverage(
	caller TestUser, victim, deleverager uint64, marketIndex uint32, baseAmount uint64,
) {
	_, err := msg.Deleverage(s.App, s.Ctx, caller, victim, deleverager, marketIndex, baseAmount)
	s.Require().NoError(err)
}

// DeleverageExpectErr asserts MsgDeleverage fails. Used to verify ADL
// invariant rejections (same-side, oversized, victim-as-deleverager).
func (s *PerpdexTestSuite) DeleverageExpectErr(
	caller TestUser, victim, deleverager uint64, marketIndex uint32, baseAmount uint64,
) error {
	_, err := msg.Deleverage(s.App, s.Ctx, caller, victim, deleverager, marketIndex, baseAmount)
	s.Require().Error(err)
	return err
}

// ---------- query shims ----------

// QueryAsset reads an asset and fails the test on error.
func (s *PerpdexTestSuite) QueryAsset(idx uint32) assettypes.Asset {
	a, err := query.Asset(s.App, s.Ctx, idx)
	s.Require().NoError(err)
	return a
}

// QueryAccount reads an account.
func (s *PerpdexTestSuite) QueryAccount(idx uint64) accounttypes.Account {
	a, err := query.Account(s.App, s.Ctx, idx)
	s.Require().NoError(err)
	return a
}

// QueryCollateral returns an account's signed collateral as math.Int.
func (s *PerpdexTestSuite) QueryCollateral(idx uint64) math.Int {
	v, err := query.Collateral(s.App, s.Ctx, idx)
	s.Require().NoError(err)
	return v
}

// QueryPosition returns the (account, market) position record.
func (s *PerpdexTestSuite) QueryPosition(accIdx uint64, marketIdx uint32) accounttypes.AccountPosition {
	p, err := query.Position(s.App, s.Ctx, accIdx, marketIdx)
	s.Require().NoError(err)
	return p
}

// QueryPositionSize is a thin wrapper that returns just the signed size.
func (s *PerpdexTestSuite) QueryPositionSize(accIdx uint64, marketIdx uint32) math.Int {
	v, err := query.PositionSize(s.App, s.Ctx, accIdx, marketIdx)
	s.Require().NoError(err)
	return v
}

// QueryMarket returns the Market struct (status / fees / expiry).
func (s *PerpdexTestSuite) QueryMarket(idx uint32) markettypes.Market {
	m, err := query.Market(s.App, s.Ctx, idx)
	s.Require().NoError(err)
	return m
}

// QueryMarketDetails returns the MarketDetails struct (margin chain,
// funding accumulators, OI, nonce ranges).
func (s *PerpdexTestSuite) QueryMarketDetails(idx uint32) markettypes.MarketDetails {
	d, err := query.MarketDetails(s.App, s.Ctx, idx)
	s.Require().NoError(err)
	return d
}

// QueryOraclePrice reads the latest aggregated oracle price for a market.
func (s *PerpdexTestSuite) QueryOraclePrice(marketIdx uint32) oracletypes.OraclePrice {
	p, err := query.OraclePrice(s.App, s.Ctx, marketIdx)
	s.Require().NoError(err)
	return p
}

// QueryOracleParams reads the current oracle module Params.
func (s *PerpdexTestSuite) QueryOracleParams() oracletypes.Params {
	p, err := query.OracleParams(s.App, s.Ctx)
	s.Require().NoError(err)
	return p
}

// QueryBestBidAsk returns the top of the book; (0,0) when empty.
func (s *PerpdexTestSuite) QueryBestBidAsk(marketIdx uint32) (uint32, uint32) {
	bid, ask, err := query.BestBidAsk(s.App, s.Ctx, marketIdx)
	s.Require().NoError(err)
	return bid, ask
}

// QueryHealthStatus returns one of the 5 health classifications for an
// account.
func (s *PerpdexTestSuite) QueryHealthStatus(accIdx uint64) uint32 {
	h, err := query.HealthStatus(s.App, s.Ctx, accIdx)
	s.Require().NoError(err)
	return h
}

// QueryLiquidationFlag returns (flag, present) for the (account, market)
// pair. `present=false` means the EndBlocker has not raised a flag (or
// it was already cleared).
func (s *PerpdexTestSuite) QueryLiquidationFlag(
	accIdx uint64, marketIdx uint32,
) (liquidationtypes.LiquidationFlag, bool) {
	flag, ok, err := query.LiquidationFlag(s.App, s.Ctx, accIdx, marketIdx)
	s.Require().NoError(err)
	return flag, ok
}

// ---------- public pool shims ----------

// CreatePublicPool spawns a regular PUBLIC_POOL sub-account under the
// signer's master and returns its index.
func (s *PerpdexTestSuite) CreatePublicPool(
	signer TestUser, masterIdx, initialTotalShares uint64,
	operatorFee, minOperatorShareRate uint32,
) uint64 {
	resp, err := msg.CreatePublicPool(s.App, s.Ctx, signer, masterIdx, initialTotalShares, operatorFee, minOperatorShareRate)
	s.Require().NoError(err)
	return resp.PoolAccountIndex
}

// CreatePublicPoolExpectErr asserts CreatePublicPool fails.
func (s *PerpdexTestSuite) CreatePublicPoolExpectErr(
	signer TestUser, masterIdx, initialTotalShares uint64,
	operatorFee, minOperatorShareRate uint32,
) error {
	_, err := msg.CreatePublicPool(s.App, s.Ctx, signer, masterIdx, initialTotalShares, operatorFee, minOperatorShareRate)
	s.Require().Error(err)
	return err
}

// UpdatePublicPool flips the pool's status / fee / min_rate.
func (s *PerpdexTestSuite) UpdatePublicPool(
	sender string, poolIdx uint64, newStatus, newFee, newMinRate uint32,
) {
	_, err := msg.UpdatePublicPool(s.App, s.Ctx, sender, poolIdx, newStatus, newFee, newMinRate)
	s.Require().NoError(err)
}

// UpdatePublicPoolExpectErr asserts UpdatePublicPool fails.
func (s *PerpdexTestSuite) UpdatePublicPoolExpectErr(
	sender string, poolIdx uint64, newStatus, newFee, newMinRate uint32,
) error {
	_, err := msg.UpdatePublicPool(s.App, s.Ctx, sender, poolIdx, newStatus, newFee, newMinRate)
	s.Require().Error(err)
	return err
}

// MintShares burns USDC from sender's master into pool shares; returns
// the share amount minted.
func (s *PerpdexTestSuite) MintShares(
	signer TestUser, poolIdx, principalAmount uint64,
) math.Int {
	resp, err := msg.MintShares(s.App, s.Ctx, signer, poolIdx, principalAmount)
	s.Require().NoError(err)
	return resp.ShareAmount
}

// MintSharesExpectErr asserts MintShares fails.
func (s *PerpdexTestSuite) MintSharesExpectErr(
	signer TestUser, poolIdx, principalAmount uint64,
) error {
	_, err := msg.MintShares(s.App, s.Ctx, signer, poolIdx, principalAmount)
	s.Require().Error(err)
	return err
}

// BurnShares redeems share_amount → uusdc.
func (s *PerpdexTestSuite) BurnShares(
	signer TestUser, poolIdx uint64, shareAmount math.Int,
) uint64 {
	resp, err := msg.BurnShares(s.App, s.Ctx, signer, poolIdx, shareAmount)
	s.Require().NoError(err)
	return resp.UsdcAmount
}

// BurnSharesExpectErr asserts BurnShares fails.
func (s *PerpdexTestSuite) BurnSharesExpectErr(
	signer TestUser, poolIdx uint64, shareAmount math.Int,
) error {
	_, err := msg.BurnShares(s.App, s.Ctx, signer, poolIdx, shareAmount)
	s.Require().Error(err)
	return err
}

// StrategyTransfer reallocates IF strategy buckets.
func (s *PerpdexTestSuite) StrategyTransfer(
	sender string, poolIdx uint64, from, to uint32, amount math.Int,
) {
	_, err := msg.StrategyTransfer(s.App, s.Ctx, sender, poolIdx, from, to, amount)
	s.Require().NoError(err)
}

// StrategyTransferExpectErr asserts StrategyTransfer fails.
func (s *PerpdexTestSuite) StrategyTransferExpectErr(
	sender string, poolIdx uint64, from, to uint32, amount math.Int,
) error {
	_, err := msg.StrategyTransfer(s.App, s.Ctx, sender, poolIdx, from, to, amount)
	s.Require().Error(err)
	return err
}

// ForceBurnShares is gov-authority break-glass burn for the LLP pool.
func (s *PerpdexTestSuite) ForceBurnShares(
	poolIdx, depositorIdx uint64, shareAmount math.Int,
) uint64 {
	resp, err := msg.ForceBurnShares(s.App, s.Ctx, s.GovAddress, poolIdx, depositorIdx, shareAmount)
	s.Require().NoError(err)
	return resp.UsdcAmount
}

// QueryPublicPoolInfo returns (info, ok). ok==false when the account is
// not a pool.
func (s *PerpdexTestSuite) QueryPublicPoolInfo(idx uint64) (accounttypes.PublicPoolInfo, bool) {
	info, ok, err := query.PublicPoolInfo(s.App, s.Ctx, idx)
	s.Require().NoError(err)
	return info, ok
}

// QueryPublicPoolShares lists the LP entries on a master account.
func (s *PerpdexTestSuite) QueryPublicPoolShares(idx uint64) []accounttypes.PublicPoolShare {
	out, err := query.PublicPoolShares(s.App, s.Ctx, idx)
	s.Require().NoError(err)
	return out
}

// QuerySharesToUSDCValue exposes the NAV math for assertions.
func (s *PerpdexTestSuite) QuerySharesToUSDCValue(poolIdx uint64, shareAmount math.Int) math.Int {
	v, err := query.SharesToUSDCValue(s.App, s.Ctx, poolIdx, shareAmount)
	s.Require().NoError(err)
	return v
}

// QueryADLQueue returns the profit-ranked ADL counterparty queue for
// the given (market, opposite_side). `oppositeIsLong=true` selects long
// candidates (the queue used when victims are short). `limit=0` defers
// to the chain's `MaxAdlCandidatesPerVictim` param.
func (s *PerpdexTestSuite) QueryADLQueue(
	marketIdx uint32, oppositeIsLong bool, limit uint32,
) []liquidationtypes.ADLCandidate {
	cands, err := query.ADLQueue(s.App, s.Ctx, marketIdx, oppositeIsLong, limit)
	s.Require().NoError(err)
	return cands
}
