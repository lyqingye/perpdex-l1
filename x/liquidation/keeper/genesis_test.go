package keeper_test

import (
	"testing"

	"cosmossdk.io/collections"

	"github.com/stretchr/testify/require"

	liqtypes "github.com/perpdex/perpdex-l1/x/liquidation/types"
)

func TestGenesis_RoundTripFlags(t *testing.T) {
	ak := newStubAccount()
	rk := newStubRisk()
	tk := &stubTrade{}
	matchk := newStubMatching()
	k, ctx := newKeeper(t, ak, rk, tk, matchk)

	flag := liqtypes.LiquidationFlag{
		AccountIndex:   100,
		MarketIndex:    7,
		FlaggedAtBlock: 42,
		FlaggedAtTime:  1_700_000,
	}
	require.NoError(t, k.InitGenesis(ctx, liqtypes.GenesisState{
		Params: liqtypes.DefaultParams(),
		Flags:  []liqtypes.LiquidationFlag{flag},
	}))

	got, err := k.Flags.Get(ctx, collections.Join(flag.AccountIndex, flag.MarketIndex))
	require.NoError(t, err)
	require.Equal(t, flag, got)

	exported, err := k.ExportGenesis(ctx)
	require.NoError(t, err)
	require.Equal(t, liqtypes.DefaultParams(), exported.Params)
	require.ElementsMatch(t, []liqtypes.LiquidationFlag{flag}, exported.Flags)
}
