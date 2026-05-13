package keeper_test

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"

	cmtprototypes "github.com/cometbft/cometbft/proto/tendermint/types"

	"cosmossdk.io/log"
	storetypes "cosmossdk.io/store/types"

	"github.com/cosmos/cosmos-sdk/runtime"
	"github.com/cosmos/cosmos-sdk/testutil/integration"
	sdk "github.com/cosmos/cosmos-sdk/types"
	moduletestutil "github.com/cosmos/cosmos-sdk/types/module/testutil"

	perptypes "github.com/perpdex/perpdex-l1/types"
	markettypes "github.com/perpdex/perpdex-l1/x/market/types"
	orderbookkeeper "github.com/perpdex/perpdex-l1/x/orderbook/keeper"
	"github.com/perpdex/perpdex-l1/x/orderbook/types"
)

// impactStubMarket lets a test pin MinInitialMarginFraction (and
// optionally QuoteMultiplier) so we can exercise the per-market
// impact-notional derivation:
//
//	impact_notional = IMPACT_USDC * MARGIN_TICK
//	                  / (MinIMF * max(QuoteMultiplier, 1))
type impactStubMarket struct {
	minIMF          uint32
	quoteMultiplier uint32
}

func (impactStubMarket) GetMarket(_ context.Context, idx uint32) (markettypes.Market, error) {
	return markettypes.Market{MarketIndex: idx, MarketType: perptypes.MarketTypePerps}, nil
}
func (s impactStubMarket) GetMarketDetails(_ context.Context, idx uint32) (markettypes.MarketDetails, error) {
	return markettypes.MarketDetails{
		MarketIndex:              idx,
		MinInitialMarginFraction: s.minIMF,
		QuoteMultiplier:          s.quoteMultiplier,
	}, nil
}
func (impactStubMarket) AllocateNonce(_ context.Context, _ uint32, _ bool) (int64, error) {
	return 1, nil
}
func (impactStubMarket) SetMarketDetails(_ context.Context, _ markettypes.MarketDetails) error {
	return nil
}

func newImpactKeeper(t *testing.T, minIMF uint32) (orderbookkeeper.Keeper, sdk.Context) {
	t.Helper()
	return newImpactKeeperWith(t, minIMF, 0)
}

func newImpactKeeperWith(t *testing.T, minIMF, quoteMultiplier uint32) (orderbookkeeper.Keeper, sdk.Context) {
	t.Helper()
	keys := storetypes.NewKVStoreKeys(types.StoreKey)
	cdc := moduletestutil.MakeTestEncodingConfig().Codec
	cms := integration.CreateMultiStore(keys, log.NewTestLogger(t))
	ctx := sdk.NewContext(cms, cmtprototypes.Header{}, true, log.NewTestLogger(t))
	k := orderbookkeeper.NewKeeper(
		cdc,
		runtime.NewKVStoreService(keys[types.StoreKey]),
		"px1xqcnyve5x5mrwwpev93xxer9venks6t29ke4l8",
		impactStubMarket{minIMF: minIMF, quoteMultiplier: quoteMultiplier},
		stubLocker{},
	)
	return k, ctx
}

// openImpactOrder is a thin wrapper around OpenOrder that builds a
// resting limit order at (price, base) on the requested side. The
// returned error is propagated so tests fail fast on insert errors.
func openImpactOrder(ctx context.Context, k orderbookkeeper.Keeper, market uint32, isAsk bool, price uint32, base uint64) error {
	idx, err := k.AllocateOrderIndex(ctx)
	if err != nil {
		return err
	}
	o := types.Order{
		OrderIndex:          idx,
		ClientOrderIndex:    0,
		OwnerAccountIndex:   1,
		MarketIndex:         market,
		IsAsk:               isAsk,
		OrderType:           perptypes.LimitOrder,
		TimeInForce:         perptypes.GTT,
		Price:               price,
		RemainingBaseAmount: base,
		Nonce:               1,
		Status:              perptypes.OrderStatusOpen,
	}
	return k.OpenOrder(ctx, o, false)
}

// TestMarketImpactNotional_PerMarketDerivation pins down the per-market
// scaling:
//
//	impact_notional = floor(IMPACT_USDC_AMOUNT * MARGIN_TICK / MinIMF)
//
// IMPACT_USDC_AMOUNT = 500_000_000 (500 USDC), MARGIN_TICK = 10_000.
// MinIMF = 500 (5% IMF / 20x leverage) -> impact_notional = 1e10.
// MinIMF = 100 (1% / 100x)              -> impact_notional = 5e10.
// MinIMF = 0 (unconfigured)             -> 0 (sentinel: insufficient depth).
func TestMarketImpactNotional_PerMarketDerivation(t *testing.T) {
	cases := []struct {
		name   string
		minIMF uint32
		want   uint64
	}{
		{"5pct_IMF", 500, 10_000_000_000},
		{"1pct_IMF", 100, 50_000_000_000},
		{"zero_unconfigured", 0, 0},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			k, ctx := newImpactKeeper(t, tc.minIMF)
			got, err := k.MarketImpactNotional(ctx, 1)
			require.NoError(t, err)
			require.Equal(t, tc.want, got)
		})
	}
}

// TestMarketImpactNotional_QuoteMultiplierDivides exercises the
// `/ QuoteMultiplier` factor in MarketImpactNotional. Today the field is
// effectively unused (resetRuntimeDetails forces 0 and no module writes
// it back), but the formula must still divide by it so activating the
// field is a localised change. QuoteMultiplier == 0 must fall back to 1
// to preserve today's behaviour bit-for-bit.
func TestMarketImpactNotional_QuoteMultiplierDivides(t *testing.T) {
	cases := []struct {
		name            string
		minIMF          uint32
		quoteMultiplier uint32
		want            uint64
	}{
		{"qm_zero_falls_back_to_1", 500, 0, 10_000_000_000},
		{"qm_one_no_op", 500, 1, 10_000_000_000},
		{"qm_two_halves", 500, 2, 5_000_000_000},
		{"qm_one_thousand", 500, 1_000, 10_000_000},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			k, ctx := newImpactKeeperWith(t, tc.minIMF, tc.quoteMultiplier)
			got, err := k.MarketImpactNotional(ctx, 1)
			require.NoError(t, err)
			require.Equal(t, tc.want, got)
		})
	}
}

// TestComputeImpactPrice_AskUsesCeilingDivision verifies that the ASK
// side rounds UP (conservative) while the BID side rounds DOWN. We
// construct a TWO-level book per side so the VWAP is non-trivial and
// ceil != floor.
//
// MinIMF = 10_000 (100% IMF) ⇒ impact_notional = 500_000_000.
//
// Ask side:
//
//	5_000 @ 50_000 → quote 250_000_000
//	5_001 @ 50_100 → quote 250_550_100
//	walker consumes the first level fully then partial-fills the
//	second to reach 5e8:
//	  needQuote = 5e8 - 2.5e8 = 2.5e8
//	  needBase  = ceil(2.5e8 / 50_100) = 4_991
//	  accBase   = 5_000 + 4_991 = 9_991
//	  accQuote  = 2.5e8 + 4_991 * 50_100 = 500_049_100
//	floor VWAP = 50_049, ceil VWAP = 50_050.
//
// Bid side: prices walked highest-first.
//
//	5_000 @ 50_000 → quote 250_000_000
//	5_001 @ 49_900 → quote 249_549_900
//	  walker: lv 50_000 fully consumed (accQuote = 2.5e8), lv 49_900
//	  partial:
//	    needQuote = 2.5e8
//	    needBase  = ceil(2.5e8 / 49_900) = 5_011
//	    accBase   = 5_000 + 5_011 = 10_011
//	    accQuote  = 2.5e8 + 5_011 * 49_900 = 500_048_900
//	  floor VWAP = 49_949, ceil VWAP = 49_950.
func TestComputeImpactPrice_AskUsesCeilingDivision(t *testing.T) {
	k, ctx := newImpactKeeper(t, 10_000)
	const market = uint32(1)

	require.NoError(t, openImpactOrder(ctx, k, market, true /*ask*/, 50_000, 5_000))
	require.NoError(t, openImpactOrder(ctx, k, market, true /*ask*/, 50_100, 5_001))

	require.NoError(t, openImpactOrder(ctx, k, market, false /*bid*/, 50_000, 5_000))
	require.NoError(t, openImpactOrder(ctx, k, market, false /*bid*/, 49_900, 6_000))

	askPx, ok, err := k.ComputeImpactPrice(ctx, market, true)
	require.NoError(t, err)
	require.True(t, ok, "ask side has enough depth")
	require.Equal(t, uint32(50_050), askPx,
		"ASK VWAP must round UP (ceil); floor=50_049, ceil=50_050")

	bidPx, ok, err := k.ComputeImpactPrice(ctx, market, false)
	require.NoError(t, err)
	require.True(t, ok, "bid side has enough depth")
	require.Equal(t, uint32(49_949), bidPx,
		"BID VWAP must round DOWN (floor); floor=49_949, ceil=49_950")
}

// TestComputeImpactPrice_InsufficientDepthReturnsFalse covers the gate
// that prevents single-side depth from producing a degenerate VWAP. A
// resting bid that absorbs only a fraction of impact_notional must
// surface (0, false, nil), letting downstream callers (the funding
// sampler and the gRPC mid) skip or clear the price.
func TestComputeImpactPrice_InsufficientDepthReturnsFalse(t *testing.T) {
	k, ctx := newImpactKeeper(t, 500) // impact_notional = 1e10
	const market = uint32(1)

	// 1 base @ 50_000 → quote = 50_000 (way below 1e10).
	require.NoError(t, openImpactOrder(ctx, k, market, false, 50_000, 1))

	bidPx, ok, err := k.ComputeImpactPrice(ctx, market, false)
	require.NoError(t, err)
	require.False(t, ok, "depth below impact_notional must report ok=false")
	require.Equal(t, uint32(0), bidPx)
}

// TestComputeImpactPrice_UnconfiguredMarketReturnsFalse exercises the
// MinIMF == 0 short-circuit: a market whose details have not been
// initialised has zero impact notional, so ComputeImpactPrice MUST
// report ok=false (insufficient depth) instead of attempting an
// orderbook walk against a zero target.
func TestComputeImpactPrice_UnconfiguredMarketReturnsFalse(t *testing.T) {
	k, ctx := newImpactKeeper(t, 0)
	const market = uint32(1)

	require.NoError(t, openImpactOrder(ctx, k, market, false, 50_000, 1_000_000_000))
	bidPx, ok, err := k.ComputeImpactPrice(ctx, market, false)
	require.NoError(t, err)
	require.False(t, ok, "MinIMF=0 ⇒ impact_notional=0 ⇒ ok=false")
	require.Equal(t, uint32(0), bidPx)
}

// TestImpactPriceRPC_HalfDepthHidesMid drives the gRPC `ImpactPrice`
// handler with a one-sided book: bid has plenty of depth, ask is
// empty. The handler MUST surface bid_ok=true / ask_ok=false and
// `impact_price = 0` rather than a half-zero mid (which would
// silently halve any consumer using this as a mark proxy).
func TestImpactPriceRPC_HalfDepthHidesMid(t *testing.T) {
	k, ctx := newImpactKeeper(t, 10_000) // impact_notional = 5e8
	const market = uint32(1)

	require.NoError(t, openImpactOrder(ctx, k, market, false, 50_000, 11_000))

	q := orderbookkeeper.NewQuerier(k)
	resp, err := q.ImpactPrice(ctx, &types.QueryImpactPriceRequest{MarketIndex: market})
	require.NoError(t, err)
	require.True(t, resp.BidOk, "bid side has sufficient depth")
	require.False(t, resp.AskOk, "ask side is empty")
	require.NotZero(t, resp.ImpactBid, "bid VWAP must surface")
	require.Zero(t, resp.ImpactAsk, "ask VWAP must be zero when ok=false")
	require.Zero(t, resp.ImpactPrice,
		"mid must be 0 when either side is missing; never a half-zero average")
}

// TestImpactPriceRPC_BothSidesComputeMid verifies the happy-path: when
// both sides resolve, the response carries `impact_price =
// floor((bid+ask)/2)` and both `ok` flags are true.
func TestImpactPriceRPC_BothSidesComputeMid(t *testing.T) {
	k, ctx := newImpactKeeper(t, 10_000) // impact_notional = 5e8
	const market = uint32(1)

	require.NoError(t, openImpactOrder(ctx, k, market, true, 50_001, 11_000))
	require.NoError(t, openImpactOrder(ctx, k, market, false, 49_999, 11_000))

	q := orderbookkeeper.NewQuerier(k)
	resp, err := q.ImpactPrice(ctx, &types.QueryImpactPriceRequest{MarketIndex: market})
	require.NoError(t, err)
	require.True(t, resp.BidOk)
	require.True(t, resp.AskOk)
	require.EqualValues(t, 49_999, resp.ImpactBid)
	require.EqualValues(t, 50_001, resp.ImpactAsk)
	require.EqualValues(t, 50_000, resp.ImpactPrice,
		"mid must equal floor((bid+ask)/2)")
}

// TestComputeImpactPrice_WalksMultipleLevels confirms the walker
// aggregates across multiple resting levels and computes the proper
// VWAP from the partial last level. We build a two-level ask book:
//
//	200_000 @ 50_000 (quote 1.0e10 = exactly impact_notional)
//	200_000 @ 50_100 (overflow level, never touched)
//
// impact_notional is fully absorbed by the first level; the VWAP is
// 50_000 with no rounding ambiguity (accQuote == 1e10 exactly).
func TestComputeImpactPrice_WalksMultipleLevels(t *testing.T) {
	k, ctx := newImpactKeeper(t, 500) // impact_notional = 1e10
	const market = uint32(1)
	require.NoError(t, openImpactOrder(ctx, k, market, true, 50_000, 200_000))
	require.NoError(t, openImpactOrder(ctx, k, market, true, 50_100, 200_000))

	askPx, ok, err := k.ComputeImpactPrice(ctx, market, true)
	require.NoError(t, err)
	require.True(t, ok)
	require.Equal(t, uint32(50_000), askPx,
		"first level absorbs the notional exactly; deeper level must be ignored")
}
