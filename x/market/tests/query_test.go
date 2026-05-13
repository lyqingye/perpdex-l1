// query_test.go exercises the gRPC query server: nil-request handling,
// listing all markets and the FilterByType disambiguation that protects
// against the proto3 default-value ambiguity where MarketTypePerps == 0
// is otherwise indistinguishable from "unset".
package tests

import (
	"testing"

	"github.com/stretchr/testify/require"

	perptypes "github.com/perpdex/perpdex-l1/types"
	"github.com/perpdex/perpdex-l1/x/market/types"
)

func TestQueryMarkets_NilRequest(t *testing.T) {
	env := newTestEnv(t)
	_, err := env.q.Markets(env.ctx, nil)
	require.Error(t, err)
}

func TestQueryMarkets_Lists(t *testing.T) {
	env := newTestEnv(t)
	_, err := env.srv.CreateMarket(env.ctx, validCreatePerpMsg(1))
	require.NoError(t, err)
	_, err = env.srv.CreateMarket(env.ctx, validCreateSpotMsg(perptypes.MinSpotMarketIndex, perptypes.LITAssetIndex))
	require.NoError(t, err)

	resp, err := env.q.Markets(env.ctx, &types.QueryMarketsRequest{})
	require.NoError(t, err)
	require.Len(t, resp.Markets, 2)
}

func TestQueryMarkets_FilterByType(t *testing.T) {
	env := newTestEnv(t)
	_, err := env.srv.CreateMarket(env.ctx, validCreatePerpMsg(1))
	require.NoError(t, err)
	_, err = env.srv.CreateMarket(env.ctx, validCreateSpotMsg(perptypes.MinSpotMarketIndex, perptypes.LITAssetIndex))
	require.NoError(t, err)

	// Spot-only filter.
	resp, err := env.q.Markets(env.ctx, &types.QueryMarketsRequest{
		FilterByType: true,
		MarketType:   perptypes.MarketTypeSpot,
	})
	require.NoError(t, err)
	require.Len(t, resp.Markets, 1)
	require.Equal(t, perptypes.MarketTypeSpot, resp.Markets[0].MarketType)

	// Perps-only filter (regression for proto3 default-value
	// ambiguity: MarketTypePerps == 0 is indistinguishable from
	// "unset", so the FilterByType flag is the only way to disambiguate).
	resp, err = env.q.Markets(env.ctx, &types.QueryMarketsRequest{
		FilterByType: true,
		MarketType:   perptypes.MarketTypePerps,
	})
	require.NoError(t, err)
	require.Len(t, resp.Markets, 1)
	require.Equal(t, perptypes.MarketTypePerps, resp.Markets[0].MarketType)

	// FilterByType=false ignores MarketType (even when set to
	// MarketTypeSpot) and returns both markets.
	resp, err = env.q.Markets(env.ctx, &types.QueryMarketsRequest{
		FilterByType: false,
		MarketType:   perptypes.MarketTypeSpot,
	})
	require.NoError(t, err)
	require.Len(t, resp.Markets, 2)
}
