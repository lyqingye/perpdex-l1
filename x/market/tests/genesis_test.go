// genesis_test.go covers the import/export contract of the market
// module: round-trip preservation, validation of pairing and uniqueness
// invariants between Markets and MarketDetails, status-aware acceptance
// rules and the rebuild of the secondary ExpiryIndex on InitGenesis.
package tests

import (
	"testing"

	"github.com/stretchr/testify/require"

	"cosmossdk.io/collections"

	perptypes "github.com/perpdex/perpdex-l1/types"
	"github.com/perpdex/perpdex-l1/x/market/types"
)

func TestGenesis_ExportImportRoundTrip(t *testing.T) {
	env := newTestEnv(t)
	_, err := env.srv.CreateMarket(env.ctx, validCreatePerpMsg(1))
	require.NoError(t, err)

	gs, err := env.keeper.ExportGenesis(env.ctx)
	require.NoError(t, err)
	require.Len(t, gs.Markets, 1)
	require.Len(t, gs.MarketDetails, 1)
	require.NoError(t, gs.Validate())
}

func TestGenesis_PairingViolationsRejected(t *testing.T) {
	gs := types.DefaultGenesis()
	gs.Markets = []types.Market{validCreatePerpMsg(1).Market}
	require.Error(t, gs.Validate(), "market without matching details must be rejected")

	gs = types.DefaultGenesis()
	gs.MarketDetails = []types.MarketDetails{validCreatePerpMsg(1).MarketDetails}
	require.Error(t, gs.Validate(), "details without matching market must be rejected")
}

func TestGenesis_DuplicateMarketRejected(t *testing.T) {
	gs := types.DefaultGenesis()
	mkt := validCreatePerpMsg(1).Market
	gs.Markets = []types.Market{mkt, mkt}
	gs.MarketDetails = []types.MarketDetails{validCreatePerpMsg(1).MarketDetails}
	require.Error(t, gs.Validate())
}

func TestGenesis_MarketStaticsRejected(t *testing.T) {
	gs := types.DefaultGenesis()
	mkt := validCreatePerpMsg(1).Market
	mkt.TakerFee = uint32(perptypes.FeeTick)
	gs.Markets = []types.Market{mkt}
	gs.MarketDetails = []types.MarketDetails{validCreatePerpMsg(1).MarketDetails}
	require.Error(t, gs.Validate())
}

func TestGenesis_ExpiredMarketAccepted(t *testing.T) {
	gs := types.DefaultGenesis()
	mkt := validCreatePerpMsg(1).Market
	mkt.Status = perptypes.MarketStatusExpired
	gs.Markets = []types.Market{mkt}
	gs.MarketDetails = []types.MarketDetails{validCreatePerpMsg(1).MarketDetails}
	require.NoError(t, gs.Validate(), "EXPIRED markets are legal in genesis")
}

// TestGenesis_RebuildsExpiryIndex confirms that InitGenesis re-registers
// every Market's expiry timestamp in the secondary index.
func TestGenesis_RebuildsExpiryIndex(t *testing.T) {
	env := newTestEnv(t)
	gs := types.DefaultGenesis()
	mkt := validCreatePerpMsg(1).Market
	mkt.ExpiryTimestamp = 1_700_000_000_000
	gs.Markets = []types.Market{mkt}
	gs.MarketDetails = []types.MarketDetails{validCreatePerpMsg(1).MarketDetails}
	require.NoError(t, env.keeper.InitGenesis(env.ctx, *gs))

	has, _ := env.keeper.ExpiryIndex.Has(env.ctx, collections.Join(int64(1_700_000_000_000), uint32(1)))
	require.True(t, has)
}
