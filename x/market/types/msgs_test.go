package types_test

import (
	"os"
	"testing"

	"cosmossdk.io/math"

	sdk "github.com/cosmos/cosmos-sdk/types"
	"github.com/stretchr/testify/require"

	perptypes "github.com/perpdex/perpdex-l1/types"
	"github.com/perpdex/perpdex-l1/x/market/types"
)

const testAuthority = "px1qv9pzxqlyckngw6zf9g9whn9d3eh4qvgsxc8cx"

// TestMain configures the chain-wide `px` bech32 prefix once per
// process so validAuth accepts the canonical test address.
func TestMain(m *testing.M) {
	cfg := sdk.GetConfig()
	cfg.SetBech32PrefixForAccount("px", "pxpub")
	cfg.SetBech32PrefixForValidator("pxvaloper", "pxvaloperpub")
	cfg.SetBech32PrefixForConsensusNode("pxvalcons", "pxvalconspub")
	os.Exit(m.Run())
}

// validPerpMarket returns a perp market record that should pass every
// statics check at the canonical defaults. Tests mutate one field per
// subcase to assert each rejection path independently.
func validPerpMarket() types.Market {
	return types.Market{
		MarketIndex:              1,
		Status:                   perptypes.MarketStatusActive,
		MarketType:               perptypes.MarketTypePerps,
		BaseAssetId:              perptypes.NativeAssetIndex,
		QuoteAssetId:             perptypes.USDCAssetIndex,
		TakerFee:                 1_000,
		MakerFee:                 500,
		LiquidationFee:           2_000,
		SizeExtensionMultiplier:  1,
		QuoteExtensionMultiplier: 1,
		MinBaseAmount:            1,
		MinQuoteAmount:           1,
		OrderQuoteLimit:          1,
		ExpiryTimestamp:          0,
	}
}

func validSpotMarket() types.Market {
	m := validPerpMarket()
	m.MarketIndex = perptypes.MinSpotMarketIndex
	m.MarketType = perptypes.MarketTypeSpot
	m.BaseAssetId = perptypes.NativeAssetIndex
	m.QuoteAssetId = perptypes.USDCAssetIndex
	return m
}

func validDetailsInit(marketIndex uint32) types.MarketDetails {
	return types.MarketDetails{
		MarketIndex:                  marketIndex,
		DefaultInitialMarginFraction: 1_000, // 10%
		MinInitialMarginFraction:     500,   // 5%
		MaintenanceMarginFraction:    250,   // 2.5%
		CloseOutMarginFraction:       125,   // 1.25%
		FundingClampSmall:            uint32(perptypes.FundingSmallClamp),
		FundingClampBig:              uint32(perptypes.FundingBigClamp),
		InterestRate:                 0,
		OpenInterestLimit:            1_000_000,
		AskNonce:                     perptypes.FirstAskNonce,
		BidNonce:                     perptypes.FirstBidNonce,
		FundingRatePrefixSum:         math.ZeroInt(),
	}
}

func validUpdateMarketMsg() *types.MsgUpdateMarket {
	return &types.MsgUpdateMarket{
		Authority:          testAuthority,
		MarketIndex:        1,
		NewStatus:          perptypes.MarketStatusActive,
		NewTakerFee:        100,
		NewMakerFee:        50,
		NewLiquidationFee:  200,
		NewMinBaseAmount:   1,
		NewMinQuoteAmount:  1,
		NewOrderQuoteLimit: 0,
		NewExpiryTimestamp: 0,
	}
}

func validUpdateMarketDetailsMsg() *types.MsgUpdateMarketDetails {
	return &types.MsgUpdateMarketDetails{
		Authority:             testAuthority,
		MarketIndex:           1,
		NewDefaultImf:         1_000,
		NewMinImf:             500,
		NewMaintenanceMf:      250,
		NewCloseOutMf:         125,
		NewFundingClampSmall:  uint32(perptypes.FundingSmallClamp),
		NewFundingClampBig:    uint32(perptypes.FundingBigClamp),
		NewInterestRate:       0,
		NewOpenInterestLimit:  1_000_000,
	}
}

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

func TestMsgUpdateMarket_Valid(t *testing.T) {
	require.NoError(t, validUpdateMarketMsg().ValidateBasic())
}

func TestMsgUpdateMarket_BadAuthority(t *testing.T) {
	m := validUpdateMarketMsg()
	m.Authority = ""
	require.Error(t, m.ValidateBasic())
}

func TestMsgUpdateMarket_NilMarketIndex(t *testing.T) {
	m := validUpdateMarketMsg()
	m.MarketIndex = perptypes.NilMarketIndex
	require.ErrorIs(t, m.ValidateBasic(), types.ErrMarketIndexExceed)
}

func TestMsgUpdateMarket_BadStatus(t *testing.T) {
	m := validUpdateMarketMsg()
	m.NewStatus = 99
	require.ErrorIs(t, m.ValidateBasic(), types.ErrInvalidMarket)
}

func TestMsgUpdateMarket_FeeTooHigh(t *testing.T) {
	cases := []struct {
		name string
		mut  func(*types.MsgUpdateMarket)
	}{
		{"taker", func(m *types.MsgUpdateMarket) { m.NewTakerFee = uint32(perptypes.FeeTick) }},
		{"maker", func(m *types.MsgUpdateMarket) { m.NewMakerFee = uint32(perptypes.FeeTick) }},
		{"liquidation", func(m *types.MsgUpdateMarket) { m.NewLiquidationFee = uint32(perptypes.FeeTick) }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			m := validUpdateMarketMsg()
			tc.mut(m)
			require.ErrorIs(t, m.ValidateBasic(), types.ErrInvalidParams)
		})
	}
}

func TestMsgUpdateMarket_ZeroMinAmounts(t *testing.T) {
	cases := []string{"base", "quote"}
	for _, name := range cases {
		t.Run(name, func(t *testing.T) {
			m := validUpdateMarketMsg()
			if name == "base" {
				m.NewMinBaseAmount = 0
			} else {
				m.NewMinQuoteAmount = 0
			}
			require.ErrorIs(t, m.ValidateBasic(), types.ErrInvalidParams)
		})
	}
}

func TestMsgUpdateMarket_NegativeExpiry(t *testing.T) {
	m := validUpdateMarketMsg()
	m.NewExpiryTimestamp = -1
	require.ErrorIs(t, m.ValidateBasic(), types.ErrInvalidMarket)
}

func TestMsgUpdateMarket_ExpiredWithFutureExpiry(t *testing.T) {
	m := validUpdateMarketMsg()
	m.NewStatus = perptypes.MarketStatusExpired
	m.NewExpiryTimestamp = 1_700_000_000_000
	require.ErrorIs(t, m.ValidateBasic(), types.ErrInvalidMarket)
}

func TestMsgUpdateMarket_NegativeOrderQuoteLimit(t *testing.T) {
	m := validUpdateMarketMsg()
	m.NewOrderQuoteLimit = -1
	require.ErrorIs(t, m.ValidateBasic(), types.ErrInvalidParams)
}

func TestMsgUpdateMarketDetails_Valid(t *testing.T) {
	require.NoError(t, validUpdateMarketDetailsMsg().ValidateBasic())
}

func TestMsgUpdateMarketDetails_BadAuthority(t *testing.T) {
	m := validUpdateMarketDetailsMsg()
	m.Authority = ""
	require.Error(t, m.ValidateBasic())
}

func TestMsgUpdateMarketDetails_NilMarketIndex(t *testing.T) {
	m := validUpdateMarketDetailsMsg()
	m.MarketIndex = perptypes.NilMarketIndex
	require.ErrorIs(t, m.ValidateBasic(), types.ErrMarketIndexExceed)
}

func TestMsgUpdateMarketDetails_MinImfZero(t *testing.T) {
	m := validUpdateMarketDetailsMsg()
	m.NewMinImf = 0
	require.ErrorIs(t, m.ValidateBasic(), types.ErrInvalidParams)
}

func TestMsgUpdateMarketDetails_IMFOverMarginTick(t *testing.T) {
	m := validUpdateMarketDetailsMsg()
	m.NewDefaultImf = uint32(perptypes.MarginTick) + 1
	require.ErrorIs(t, m.ValidateBasic(), types.ErrInvalidParams)
}

func TestMsgUpdateMarketDetails_MarginChainViolated(t *testing.T) {
	m := validUpdateMarketDetailsMsg()
	m.NewMaintenanceMf = m.NewDefaultImf // violates strict <
	require.ErrorIs(t, m.ValidateBasic(), types.ErrInvalidMarginChain)
}

func TestMsgUpdateMarketDetails_FundingClampReversed(t *testing.T) {
	m := validUpdateMarketDetailsMsg()
	m.NewFundingClampSmall = m.NewFundingClampBig + 1
	require.ErrorIs(t, m.ValidateBasic(), types.ErrInvalidParams)
}

func TestMsgUpdateMarketDetails_InterestRateTooHigh(t *testing.T) {
	m := validUpdateMarketDetailsMsg()
	m.NewInterestRate = uint32(perptypes.FundingRateTick + 1)
	require.ErrorIs(t, m.ValidateBasic(), types.ErrInvalidParams)
}

func TestMsgUpdateMarketDetails_OpenInterestLimitTooHigh(t *testing.T) {
	m := validUpdateMarketDetailsMsg()
	m.NewOpenInterestLimit = perptypes.MaxOrderBaseAmount + 1
	require.ErrorIs(t, m.ValidateBasic(), types.ErrInvalidParams)
}

func TestMsgUpdateParams_Valid(t *testing.T) {
	m := &types.MsgUpdateParams{
		Authority: testAuthority,
		Params:    types.DefaultParams(),
	}
	require.NoError(t, m.ValidateBasic())
}

func TestMsgUpdateParams_BadAuthority(t *testing.T) {
	m := &types.MsgUpdateParams{
		Authority: "",
		Params:    types.DefaultParams(),
	}
	require.Error(t, m.ValidateBasic())
}

func TestMsgUpdateParams_PerpsOverlapSpot(t *testing.T) {
	m := &types.MsgUpdateParams{
		Authority: testAuthority,
		Params: types.Params{
			MaxPerpsMarketIndex:       perptypes.MinSpotMarketIndex,
			MinSpotMarketIndex:        perptypes.MinSpotMarketIndex,
			MaxSpotMarketIndex:        perptypes.MaxSpotMarketIndex,
			MaxMarketsExpiredPerBlock: 32,
		},
	}
	require.ErrorIs(t, m.ValidateBasic(), types.ErrInvalidParams)
}

func TestMsgUpdateParams_PerpsAboveNilMarketIndex(t *testing.T) {
	m := &types.MsgUpdateParams{
		Authority: testAuthority,
		Params: types.Params{
			MaxPerpsMarketIndex:       perptypes.NilMarketIndex,
			MinSpotMarketIndex:        perptypes.MinSpotMarketIndex,
			MaxSpotMarketIndex:        perptypes.MaxSpotMarketIndex,
			MaxMarketsExpiredPerBlock: 32,
		},
	}
	require.ErrorIs(t, m.ValidateBasic(), types.ErrInvalidParams)
}

func TestMsgUpdateParams_ZeroSpotIndex(t *testing.T) {
	m := &types.MsgUpdateParams{
		Authority: testAuthority,
		Params: types.Params{
			MaxPerpsMarketIndex:       0,
			MinSpotMarketIndex:        0,
			MaxSpotMarketIndex:        0,
			MaxMarketsExpiredPerBlock: 32,
		},
	}
	require.ErrorIs(t, m.ValidateBasic(), types.ErrInvalidParams)
}

func TestMsgUpdateParams_MaxMarketsZero(t *testing.T) {
	// 0 is a valid emergency switch — auto-expiry disabled.
	m := &types.MsgUpdateParams{
		Authority: testAuthority,
		Params: types.Params{
			MaxPerpsMarketIndex:       perptypes.MaxPerpsMarketIndex,
			MinSpotMarketIndex:        perptypes.MinSpotMarketIndex,
			MaxSpotMarketIndex:        perptypes.MaxSpotMarketIndex,
			MaxMarketsExpiredPerBlock: 0,
		},
	}
	require.NoError(t, m.ValidateBasic())
}

func TestParams_IsValidIndex(t *testing.T) {
	p := types.DefaultParams()
	require.True(t, p.IsValidIndex(1))
	require.True(t, p.IsValidIndex(perptypes.MaxPerpsMarketIndex))
	require.True(t, p.IsValidIndex(perptypes.MinSpotMarketIndex))
	require.True(t, p.IsValidIndex(perptypes.MaxSpotMarketIndex))
	require.False(t, p.IsValidIndex(perptypes.NilMarketIndex))
	require.False(t, p.IsValidIndex(perptypes.MaxPerpsMarketIndex+1))
}
