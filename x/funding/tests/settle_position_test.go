// settle_position_test.go covers `SettlePositionFunding` — the
// per-position funding settlement entry-point invoked by external
// modules (matching, liquidation) whenever a position needs its
// LastFundingRatePrefixSum snapshot reconciled against the latest
// market-wide prefix sum.
package tests

import (
	"testing"

	"github.com/stretchr/testify/require"

	"cosmossdk.io/math"

	perptypes "github.com/perpdex/perpdex-l1/types"
	accounttypes "github.com/perpdex/perpdex-l1/x/account/types"
	markettypes "github.com/perpdex/perpdex-l1/x/market/types"
	oracletypes "github.com/perpdex/perpdex-l1/x/oracle/types"
)

// TestSettlePositionFunding_ZeroPositionIsNoOp pins the post-#91
// contract: SettlePositionFunding on an empty / fully-closed row is
// a no-op. Closed rows are removed from storage (or, when leverage
// is non-default, retained as a leverage-only config row without a
// funding obligation); the next OpenPosition in x/trade seeds
// LastFundingRatePrefixSum from the market's current value, so the
// funding keeper no longer needs to keep an "empty" snapshot in
// sync.
func TestSettlePositionFunding_ZeroPositionIsNoOp(t *testing.T) {
	const (
		accountIndex = uint64(7)
		marketIndex  = uint32(1)
	)

	mk := &stubMarket{
		markets: map[uint32]markettypes.Market{
			marketIndex: {MarketIndex: marketIndex, MarketType: perptypes.MarketTypePerps, Status: perptypes.MarketStatusActive},
		},
		details: map[uint32]markettypes.MarketDetails{
			marketIndex: {
				MarketIndex:          marketIndex,
				FundingRatePrefixSum: math.NewInt(100_000_000),
				AggregatePremiumSum:  math.ZeroInt(),
			},
		},
	}
	ak := newStatefulAccount()
	k, ctx := newFundingKeeperWithAccount(
		t,
		mk,
		stubOracle{price: oracletypes.OraclePrice{IndexPrice: 49_500, MarkPrice: 50_000}},
		stubBook{},
		ak,
	)

	require.NoError(t, k.SettlePositionFunding(ctx, accountIndex, marketIndex))
	key := [2]uint64{accountIndex, uint64(marketIndex)}
	_, ok := ak.positions[key]
	require.False(t, ok, "SettlePositionFunding on empty must NOT persist a position row")

	// Simulate ApplyPerpsMatching opening a position that's already
	// been seeded with the market's current prefix (the x/trade
	// `openHandler` is responsible for this seed). Advance the
	// prefix by only 20_000_000 and confirm the next settlement
	// charges that delta only.
	ak.positions[key] = accounttypes.AccountPosition{
		AccountIndex:             accountIndex,
		MarketIndex:              marketIndex,
		BaseSize:                 math.NewInt(1_000_000),
		EntryQuote:               math.ZeroInt(),
		LastFundingRatePrefixSum: math.NewInt(100_000_000),
		AllocatedMargin:          math.ZeroInt(),
	}
	d := mk.details[marketIndex]
	d.FundingRatePrefixSum = math.NewInt(120_000_000)
	mk.details[marketIndex] = d

	require.NoError(t, k.SettlePositionFunding(ctx, accountIndex, marketIndex))
	settled := ak.positions[key]
	require.EqualValues(t, 20_000_000, settled.EntryQuote.Int64())
	require.EqualValues(t, 120_000_000, settled.LastFundingRatePrefixSum.Int64())
}
