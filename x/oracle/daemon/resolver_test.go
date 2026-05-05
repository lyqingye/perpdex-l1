package daemon_test

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/perpdex/perpdex-l1/x/oracle/daemon"
	"github.com/perpdex/perpdex-l1/x/oracle/types"
)

type fakeMarkets struct {
	markets []types.MarketShim
}

func (f fakeMarkets) IterateMarkets(_ context.Context, cb func(types.MarketShim) bool) error {
	for _, m := range f.markets {
		if cb(m) {
			return nil
		}
	}
	return nil
}

type fakeAssets struct {
	displayName map[uint32]string
	decimals    map[uint32]uint32
}

func (f fakeAssets) GetAssetByIndex(_ context.Context, idx uint32) (string, uint32, error) {
	return f.displayName[idx], f.decimals[idx], nil
}

func TestResolverRefreshMaps(t *testing.T) {
	mr := fakeMarkets{markets: []types.MarketShim{
		{MarketIndex: 1, BaseAssetID: 10, QuoteAssetID: 20, Decimals: 2},
		{MarketIndex: 2, BaseAssetID: 11, QuoteAssetID: 20, Decimals: 4},
	}}
	ar := fakeAssets{displayName: map[uint32]string{
		10: "BTC", 11: "ETH", 20: "USDT",
	}}
	r := daemon.NewMarketResolver()
	require.NoError(t, r.Refresh(context.Background(), mr, ar))

	idx, ok := r.MarketIndex("BTC/USD")
	require.True(t, ok, "USDT should normalise to USD")
	require.EqualValues(t, 1, idx)

	idx, ok = r.MarketIndex("eth/usd")
	require.True(t, ok)
	require.EqualValues(t, 2, idx)

	require.EqualValues(t, 2, r.Decimals(1))
	require.EqualValues(t, 4, r.Decimals(2))
	require.Equal(t, "BTC/USD", r.Pair(1))
}

func TestResolverDecimalsFallback(t *testing.T) {
	r := daemon.NewMarketResolver()
	r.Set("BTC/USD", 1, 0)
	require.EqualValues(t, 8, r.Decimals(1), "zero decimals must fall back to 8")
}

func TestPairFromAssets(t *testing.T) {
	require.Equal(t, "BTC/USD", daemon.PairFromAssets("btc", "usdt"))
	require.Equal(t, "BTC/USD", daemon.PairFromAssets("BTC", "USD"))
	require.Equal(t, "ETH/EUR", daemon.PairFromAssets(" eth ", " EUR "))
}
