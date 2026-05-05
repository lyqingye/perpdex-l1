package oracle

import (
	"math/big"
	"testing"
	"time"

	"github.com/perpdex/perpdex-l1/oracle-sidecar/providers/types"
	"github.com/stretchr/testify/require"
)

func TestMedianOdd(t *testing.T) {
	got := Median([]*big.Int{big.NewInt(1), big.NewInt(5), big.NewInt(3)})
	require.Zero(t, got.Cmp(big.NewInt(3)))
}

func TestMedianEvenPicksLowerMid(t *testing.T) {
	got := Median([]*big.Int{big.NewInt(1), big.NewInt(2), big.NewInt(3), big.NewInt(4)})
	require.Zero(t, got.Cmp(big.NewInt(2)))
}

func TestMedianEmpty(t *testing.T) {
	require.Nil(t, Median(nil))
}

func TestAggregateFiltersStale(t *testing.T) {
	now := time.Date(2026, 5, 5, 19, 0, 0, 0, time.UTC)
	pair, _ := types.ParseCurrencyPair("BTC/USD")
	in := []AggregateInputs{{
		Pair: pair,
		Samples: []types.Price{
			{Pair: pair, Value: big.NewInt(100), Timestamp: now.Add(-30 * time.Second)},
			{Pair: pair, Value: big.NewInt(200), Timestamp: now.Add(-1 * time.Second)},
			{Pair: pair, Value: big.NewInt(220), Timestamp: now.Add(-1 * time.Second)},
		},
	}}
	out := Aggregate(in, AggregateConfig{MaxAge: 5 * time.Second, NowOverride: now})
	require.Len(t, out, 1)
	require.Zero(t, out["BTC/USD"].Cmp(big.NewInt(200)), "expected median 200, got %s", out["BTC/USD"].String())
}

func TestAggregateMinSources(t *testing.T) {
	now := time.Date(2026, 5, 5, 19, 0, 0, 0, time.UTC)
	pair, _ := types.ParseCurrencyPair("BTC/USD")
	in := []AggregateInputs{{
		Pair: pair,
		Samples: []types.Price{
			{Pair: pair, Value: big.NewInt(100), Timestamp: now},
		},
	}}
	out := Aggregate(in, AggregateConfig{MaxAge: 5 * time.Second, MinSources: 2, NowOverride: now})
	require.Empty(t, out)
}

func TestAggregateDropsZeroOrNegative(t *testing.T) {
	now := time.Date(2026, 5, 5, 19, 0, 0, 0, time.UTC)
	pair, _ := types.ParseCurrencyPair("BTC/USD")
	in := []AggregateInputs{{
		Pair: pair,
		Samples: []types.Price{
			{Pair: pair, Value: big.NewInt(0), Timestamp: now},
			{Pair: pair, Value: big.NewInt(-5), Timestamp: now},
			{Pair: pair, Value: big.NewInt(50), Timestamp: now},
		},
	}}
	out := Aggregate(in, AggregateConfig{MaxAge: 5 * time.Second, NowOverride: now})
	require.Len(t, out, 1)
	require.Zero(t, out["BTC/USD"].Cmp(big.NewInt(50)))
}
