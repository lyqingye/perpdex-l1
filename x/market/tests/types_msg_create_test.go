// types_msg_create_test.go pins MsgCreateMarket.ValidateBasic across
// every rejection path: authority shape, index range, market status,
// market type, fee tick alignment, min-amount guards, extension
// multipliers, spot/perp asset-pair invariants, expiry sign, margin
// chain ordering, funding clamps, nonce init values and the
// non-zero-runtime-field defense-in-depth check.
package tests

import (
	"testing"

	"cosmossdk.io/math"
	"github.com/stretchr/testify/require"

	perptypes "github.com/perpdex/perpdex-l1/types"
	"github.com/perpdex/perpdex-l1/x/market/types"
)

func TestMsgCreateMarket_Valid(t *testing.T) {
	mkt := validPerpMarket()
	det := validDetailsInit(mkt.MarketIndex)
	m := &types.MsgCreateMarket{Authority: testAuthority, Market: mkt, MarketDetails: det}
	require.NoError(t, m.ValidateBasic())
}

func TestMsgCreateMarket_ValidSpot(t *testing.T) {
	mkt := validSpotMarket()
	det := validDetailsInit(mkt.MarketIndex)
	m := &types.MsgCreateMarket{Authority: testAuthority, Market: mkt, MarketDetails: det}
	require.NoError(t, m.ValidateBasic())
}

func TestMsgCreateMarket_BadAuthority(t *testing.T) {
	mkt := validPerpMarket()
	det := validDetailsInit(mkt.MarketIndex)
	m := &types.MsgCreateMarket{Authority: "not-bech32", Market: mkt, MarketDetails: det}
	require.Error(t, m.ValidateBasic())
}

func TestMsgCreateMarket_MarketDetailsIndexMismatch(t *testing.T) {
	mkt := validPerpMarket()
	det := validDetailsInit(mkt.MarketIndex + 1)
	m := &types.MsgCreateMarket{Authority: testAuthority, Market: mkt, MarketDetails: det}
	require.ErrorIs(t, m.ValidateBasic(), types.ErrInvalidMarket)
}

func TestMsgCreateMarket_RejectsNonActiveStatus(t *testing.T) {
	mkt := validPerpMarket()
	mkt.Status = perptypes.MarketStatusExpired
	det := validDetailsInit(mkt.MarketIndex)
	m := &types.MsgCreateMarket{Authority: testAuthority, Market: mkt, MarketDetails: det}
	require.ErrorIs(t, m.ValidateBasic(), types.ErrInvalidMarket)
}

func TestMsgCreateMarket_PerpsIndexOutOfRange(t *testing.T) {
	mkt := validPerpMarket()
	mkt.MarketIndex = perptypes.MaxPerpsMarketIndex + 1
	det := validDetailsInit(mkt.MarketIndex)
	m := &types.MsgCreateMarket{Authority: testAuthority, Market: mkt, MarketDetails: det}
	require.ErrorIs(t, m.ValidateBasic(), types.ErrMarketIndexExceed)
}

func TestMsgCreateMarket_SpotIndexOutOfRange(t *testing.T) {
	mkt := validSpotMarket()
	mkt.MarketIndex = perptypes.MaxSpotMarketIndex + 1
	det := validDetailsInit(mkt.MarketIndex)
	m := &types.MsgCreateMarket{Authority: testAuthority, Market: mkt, MarketDetails: det}
	require.ErrorIs(t, m.ValidateBasic(), types.ErrMarketIndexExceed)
}

func TestMsgCreateMarket_NilMarketIndex(t *testing.T) {
	mkt := validPerpMarket()
	mkt.MarketIndex = perptypes.NilMarketIndex
	det := validDetailsInit(mkt.MarketIndex)
	m := &types.MsgCreateMarket{Authority: testAuthority, Market: mkt, MarketDetails: det}
	require.ErrorIs(t, m.ValidateBasic(), types.ErrMarketIndexExceed)
}

func TestMsgCreateMarket_UnknownMarketType(t *testing.T) {
	mkt := validPerpMarket()
	mkt.MarketType = 99
	det := validDetailsInit(mkt.MarketIndex)
	m := &types.MsgCreateMarket{Authority: testAuthority, Market: mkt, MarketDetails: det}
	require.ErrorIs(t, m.ValidateBasic(), types.ErrInvalidMarket)
}

func TestMsgCreateMarket_FeeOverTick(t *testing.T) {
	cases := []struct {
		name string
		mut  func(*types.Market)
	}{
		{"taker_fee", func(m *types.Market) { m.TakerFee = uint32(perptypes.FeeTick) }},
		{"maker_fee", func(m *types.Market) { m.MakerFee = uint32(perptypes.FeeTick) }},
		{"liquidation_fee", func(m *types.Market) { m.LiquidationFee = uint32(perptypes.FeeTick) }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			mkt := validPerpMarket()
			tc.mut(&mkt)
			det := validDetailsInit(mkt.MarketIndex)
			m := &types.MsgCreateMarket{Authority: testAuthority, Market: mkt, MarketDetails: det}
			require.ErrorIs(t, m.ValidateBasic(), types.ErrInvalidParams)
		})
	}
}

func TestMsgCreateMarket_ZeroMinAmounts(t *testing.T) {
	cases := []struct {
		name string
		mut  func(*types.Market)
	}{
		{"min_base", func(m *types.Market) { m.MinBaseAmount = 0 }},
		{"min_quote", func(m *types.Market) { m.MinQuoteAmount = 0 }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			mkt := validPerpMarket()
			tc.mut(&mkt)
			det := validDetailsInit(mkt.MarketIndex)
			m := &types.MsgCreateMarket{Authority: testAuthority, Market: mkt, MarketDetails: det}
			require.ErrorIs(t, m.ValidateBasic(), types.ErrInvalidParams)
		})
	}
}

func TestMsgCreateMarket_NegativeOrderQuoteLimit(t *testing.T) {
	mkt := validPerpMarket()
	mkt.OrderQuoteLimit = -1
	det := validDetailsInit(mkt.MarketIndex)
	m := &types.MsgCreateMarket{Authority: testAuthority, Market: mkt, MarketDetails: det}
	require.ErrorIs(t, m.ValidateBasic(), types.ErrInvalidParams)
}

func TestMsgCreateMarket_ZeroExtensionMultipliers(t *testing.T) {
	cases := []string{"size", "quote"}
	for _, name := range cases {
		t.Run(name, func(t *testing.T) {
			mkt := validPerpMarket()
			if name == "size" {
				mkt.SizeExtensionMultiplier = 0
			} else {
				mkt.QuoteExtensionMultiplier = 0
			}
			det := validDetailsInit(mkt.MarketIndex)
			m := &types.MsgCreateMarket{Authority: testAuthority, Market: mkt, MarketDetails: det}
			require.ErrorIs(t, m.ValidateBasic(), types.ErrInvalidParams)
		})
	}
}

func TestMsgCreateMarket_SpotRejectsSameBaseQuote(t *testing.T) {
	mkt := validSpotMarket()
	mkt.BaseAssetId = mkt.QuoteAssetId
	det := validDetailsInit(mkt.MarketIndex)
	m := &types.MsgCreateMarket{Authority: testAuthority, Market: mkt, MarketDetails: det}
	require.ErrorIs(t, m.ValidateBasic(), types.ErrInvalidMarket)
}

func TestMsgCreateMarket_RejectsNegativeExpiry(t *testing.T) {
	mkt := validPerpMarket()
	mkt.ExpiryTimestamp = -1
	det := validDetailsInit(mkt.MarketIndex)
	m := &types.MsgCreateMarket{Authority: testAuthority, Market: mkt, MarketDetails: det}
	require.ErrorIs(t, m.ValidateBasic(), types.ErrInvalidMarket)
}

func TestMsgCreateMarket_MarginChainViolations(t *testing.T) {
	cases := []struct {
		name string
		mut  func(*types.MarketDetails)
	}{
		{"min_imf_zero", func(d *types.MarketDetails) { d.MinInitialMarginFraction = 0 }},
		{"default_below_min", func(d *types.MarketDetails) { d.DefaultInitialMarginFraction = 100; d.MinInitialMarginFraction = 200 }},
		{"maintenance_above_default", func(d *types.MarketDetails) { d.MaintenanceMarginFraction = d.DefaultInitialMarginFraction }},
		{"close_out_above_maintenance", func(d *types.MarketDetails) { d.CloseOutMarginFraction = d.MaintenanceMarginFraction }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			mkt := validPerpMarket()
			det := validDetailsInit(mkt.MarketIndex)
			tc.mut(&det)
			m := &types.MsgCreateMarket{Authority: testAuthority, Market: mkt, MarketDetails: det}
			err := m.ValidateBasic()
			require.Error(t, err)
		})
	}
}

func TestMsgCreateMarket_IMFOverMarginTick(t *testing.T) {
	mkt := validPerpMarket()
	det := validDetailsInit(mkt.MarketIndex)
	det.DefaultInitialMarginFraction = uint32(perptypes.MarginTick) + 1
	m := &types.MsgCreateMarket{Authority: testAuthority, Market: mkt, MarketDetails: det}
	require.ErrorIs(t, m.ValidateBasic(), types.ErrInvalidParams)
}

func TestMsgCreateMarket_FundingClampReversed(t *testing.T) {
	mkt := validPerpMarket()
	det := validDetailsInit(mkt.MarketIndex)
	det.FundingClampSmall = 1_000
	det.FundingClampBig = 100
	m := &types.MsgCreateMarket{Authority: testAuthority, Market: mkt, MarketDetails: det}
	require.ErrorIs(t, m.ValidateBasic(), types.ErrInvalidParams)
}

func TestMsgCreateMarket_RejectsNonInitNonce(t *testing.T) {
	mkt := validPerpMarket()
	det := validDetailsInit(mkt.MarketIndex)
	det.AskNonce = perptypes.FirstAskNonce + 1
	m := &types.MsgCreateMarket{Authority: testAuthority, Market: mkt, MarketDetails: det}
	require.ErrorIs(t, m.ValidateBasic(), types.ErrNonceExhausted)
}

func TestMsgCreateMarket_RejectsNonZeroRuntimeFields(t *testing.T) {
	cases := []struct {
		name string
		mut  func(*types.MarketDetails)
	}{
		{"open_interest", func(d *types.MarketDetails) { d.OpenInterest = 1 }},
		{"mark_price", func(d *types.MarketDetails) { d.MarkPrice = 1 }},
		{"impact_price", func(d *types.MarketDetails) { d.ImpactPrice = 1 }},
		{"premium_sum", func(d *types.MarketDetails) { d.AggregatePremiumSum = math.NewInt(1) }},
		{"total_order_count", func(d *types.MarketDetails) { d.TotalOrderCount = 1 }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			mkt := validPerpMarket()
			det := validDetailsInit(mkt.MarketIndex)
			tc.mut(&det)
			m := &types.MsgCreateMarket{Authority: testAuthority, Market: mkt, MarketDetails: det}
			require.ErrorIs(t, m.ValidateBasic(), types.ErrInvalidParams)
		})
	}
}
