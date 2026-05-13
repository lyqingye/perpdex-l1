// query_test.go covers the funding module's gRPC query handlers
// (`x/funding/keeper/grpc_query.go`).
package tests

import (
	"testing"

	"github.com/stretchr/testify/require"

	"cosmossdk.io/math"

	perptypes "github.com/perpdex/perpdex-l1/types"
	fundingkeeper "github.com/perpdex/perpdex-l1/x/funding/keeper"
	fundingtypes "github.com/perpdex/perpdex-l1/x/funding/types"
	markettypes "github.com/perpdex/perpdex-l1/x/market/types"
	oracletypes "github.com/perpdex/perpdex-l1/x/oracle/types"
)

// TestMarketFundingRateQuery_ReturnsGlobalFundingRoundTimestamp pins
// that the MarketFundingRate query exposes the *global*
// `LastFundingRoundTimestamp` (from FundingMetadata) and NOT any
// per-market sample timestamp.
func TestMarketFundingRateQuery_ReturnsGlobalFundingRoundTimestamp(t *testing.T) {
	mk := &stubMarket{
		markets: map[uint32]markettypes.Market{
			1: {MarketIndex: 1, MarketType: perptypes.MarketTypePerps, Status: perptypes.MarketStatusActive},
		},
		details: map[uint32]markettypes.MarketDetails{
			1: {
				MarketIndex:                1,
				FundingRatePrefixSum:       math.NewInt(123),
				AggregatePremiumSum:        math.ZeroInt(),
				LastPremiumSampleTimestamp: 456,
			},
		},
	}
	k, ctx := newFundingKeeper(
		t,
		mk,
		stubOracle{price: oracletypes.OraclePrice{IndexPrice: 49_500, MarkPrice: 50_000}},
		stubBook{},
	)
	require.NoError(t, k.Metadata.Set(ctx, fundingtypes.FundingMetadata{
		LastFundingRoundTimestamp: 789,
	}))

	resp, err := fundingkeeper.NewQuerier(k).MarketFundingRate(ctx, &fundingtypes.QueryMarketFundingRateRequest{MarketIndex: 1})
	require.NoError(t, err)
	require.EqualValues(t, 123, resp.PrefixSum.Int64())
	require.EqualValues(t, 789, resp.LastSettledAt)
}
