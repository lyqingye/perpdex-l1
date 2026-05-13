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
	markettypes "github.com/perpdex/perpdex-l1/x/market/types"
	oracletypes "github.com/perpdex/perpdex-l1/x/oracle/types"
)

// TestSettlePositionFunding_ZeroPositionSnapshotsCurrentPrefix ensures
// a fresh or fully closed position snapshots the current funding
// prefix so it does not inherit any prefix accumulated before it
// opened.
func TestSettlePositionFunding_ZeroPositionSnapshotsCurrentPrefix(t *testing.T) {
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
	snapshotted := ak.positions[key]
	require.True(t, snapshotted.BaseSize.IsZero())
	require.EqualValues(t, 100_000_000, snapshotted.LastFundingRatePrefixSum.Int64())

	// Simulate ApplyPerpsMatching opening a new position after the
	// zero-size settle above, then advance the market prefix by only
	// 20_000_000. The next funding settlement must charge that new
	// delta only, not the full 120_000_000 prefix.
	snapshotted.BaseSize = math.NewInt(1_000_000)
	snapshotted.EntryQuote = math.ZeroInt()
	ak.positions[key] = snapshotted
	d := mk.details[marketIndex]
	d.FundingRatePrefixSum = math.NewInt(120_000_000)
	mk.details[marketIndex] = d

	require.NoError(t, k.SettlePositionFunding(ctx, accountIndex, marketIndex))
	settled := ak.positions[key]
	require.EqualValues(t, 20_000_000, settled.EntryQuote.Int64())
	require.EqualValues(t, 120_000_000, settled.LastFundingRatePrefixSum.Int64())
}
