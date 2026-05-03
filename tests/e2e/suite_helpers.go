package e2e

import (
	sdk "github.com/cosmos/cosmos-sdk/types"

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

// AddOracleProvider whitelists `provider` so it can call MsgInjectOracle.
func (s *PerpdexTestSuite) AddOracleProvider(provider sdk.AccAddress, description string) {
	_, err := msg.AddOracleProvider(s.App, s.Ctx, s.GovAddress, provider, description)
	s.Require().NoError(err)
}

// SetAggregationMode flips the oracle between WHITELIST and PoS_MEDIAN.
func (s *PerpdexTestSuite) SetAggregationMode(mode uint32) {
	_, err := msg.SetAggregationMode(s.App, s.Ctx, s.GovAddress, mode)
	s.Require().NoError(err)
}

// InjectPrice posts a single (market_index, mark, index) point. Most tests
// drive a single market at a time; multi-market scenarios should call the
// underlying msg.InjectPrice directly.
func (s *PerpdexTestSuite) InjectPrice(provider sdk.AccAddress, marketIndex uint32, indexPrice, markPrice uint32) {
	_, err := msg.InjectPrice(s.App, s.Ctx, provider, []oracletypes.MarketPrice{{
		MarketIndex: marketIndex,
		IndexPrice:  indexPrice,
		MarkPrice:   markPrice,
	}})
	s.Require().NoError(err)
}

// AggregateVotes drives the PoS_MEDIAN code path by posting an
// already-aggregated MsgAggregateOracleVotes (signed by the gov authority).
func (s *PerpdexTestSuite) AggregateVotes(
	height int64,
	aggregations []oracletypes.MarketAggregation,
	voters []oracletypes.VoterRecord,
) {
	_, err := msg.AggregateVotes(s.App, s.Ctx, s.GovAddress, height, aggregations, voters)
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
