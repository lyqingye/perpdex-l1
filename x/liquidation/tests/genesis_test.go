// Genesis round-trip behaviour for the liquidation module: persisting
// `Params` via InitGenesis and re-exporting them with ExportGenesis
// must yield a byte-for-byte equivalent state.
package tests

import (
	"testing"

	"github.com/stretchr/testify/require"

	liqtypes "github.com/perpdex/perpdex-l1/x/liquidation/types"
)

func TestGenesis_RoundTripParams(t *testing.T) {
	ak := newStubAccount()
	rk := newStubRisk()
	tk := &stubTrade{}
	matchk := newStubMatching()
	k, ctx := newKeeper(t, ak, rk, tk, matchk)

	custom := liqtypes.Params{
		MaxAdlAttemptsPerBlock:    3,
		MaxAdlCandidatesPerVictim: 11,
	}
	require.NoError(t, k.InitGenesis(ctx, liqtypes.GenesisState{Params: custom}))

	exported, err := k.ExportGenesis(ctx)
	require.NoError(t, err)
	require.Equal(t, custom, exported.Params)
}
