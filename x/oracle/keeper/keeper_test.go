package keeper_test

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	cmtprototypes "github.com/cometbft/cometbft/proto/tendermint/types"

	"cosmossdk.io/log"
	storetypes "cosmossdk.io/store/types"

	"github.com/cosmos/cosmos-sdk/runtime"
	"github.com/cosmos/cosmos-sdk/testutil/integration"
	sdk "github.com/cosmos/cosmos-sdk/types"
	moduletestutil "github.com/cosmos/cosmos-sdk/types/module/testutil"

	oraclekeeper "github.com/perpdex/perpdex-l1/x/oracle/keeper"
	oracletypes "github.com/perpdex/perpdex-l1/x/oracle/types"
)

func newOracleKeeper(t *testing.T) (oraclekeeper.Keeper, sdk.Context) {
	t.Helper()
	keys := storetypes.NewKVStoreKeys(oracletypes.StoreKey)
	cdc := moduletestutil.MakeTestEncodingConfig().Codec
	cms := integration.CreateMultiStore(keys, log.NewTestLogger(t))
	ctx := sdk.NewContext(cms, cmtprototypes.Header{Time: time.Unix(1_700_000_000, 0)}, true, log.NewTestLogger(t))
	k := oraclekeeper.NewKeeper(
		cdc,
		runtime.NewKVStoreService(keys[oracletypes.StoreKey]),
		"auth",
	)
	require.NoError(t, k.Params.Set(ctx, oracletypes.DefaultParams()))
	return k, ctx
}

// TestSetPrice_RejectsZero ensures runtime price writes cannot slip zero
// mark / index prices into state (audit Medium oracle-12).
func TestSetPrice_RejectsZero(t *testing.T) {
	k, ctx := newOracleKeeper(t)
	err := k.SetPrice(ctx, oracletypes.OraclePrice{MarketIndex: 1, IndexPrice: 0, MarkPrice: 10})
	require.ErrorIs(t, err, oracletypes.ErrInvalidPrice)
	err = k.SetPrice(ctx, oracletypes.OraclePrice{MarketIndex: 1, IndexPrice: 10, MarkPrice: 0})
	require.ErrorIs(t, err, oracletypes.ErrInvalidPrice)
}

// TestGetPrice_StaleByAge rejects prices older than Params.MaxAgeMs.
func TestGetPrice_StaleByAge(t *testing.T) {
	k, ctx := newOracleKeeper(t)
	// Price updated 10 minutes before block time.
	blockTs := ctx.BlockTime().UnixMilli()
	require.NoError(t, k.SetPrice(ctx, oracletypes.OraclePrice{
		MarketIndex:          5,
		IndexPrice:           100,
		MarkPrice:            100,
		LastUpdatedTimestamp: blockTs - 600_000,
	}))
	_, err := k.GetPrice(ctx, 5)
	require.ErrorIs(t, err, oracletypes.ErrStalePrice)

	stored, err := k.GetStoredPrice(ctx, 5)
	require.NoError(t, err)
	require.EqualValues(t, 100, stored.MarkPrice)
}

// TestGetPrice_NeverUpdated rejects a stored price whose timestamp is
// still zero (placeholder / genesis seed).
func TestGetPrice_NeverUpdated(t *testing.T) {
	k, ctx := newOracleKeeper(t)
	require.NoError(t, k.SetPriceUnsafe(ctx, oracletypes.OraclePrice{
		MarketIndex: 7, IndexPrice: 1, MarkPrice: 1,
	}))
	_, err := k.GetPrice(ctx, 7)
	require.ErrorIs(t, err, oracletypes.ErrStalePrice)
}

// TestGetPrice_Fresh returns the price when it is still within
// MaxAgeMs.
func TestGetPrice_Fresh(t *testing.T) {
	k, ctx := newOracleKeeper(t)
	blockTs := ctx.BlockTime().UnixMilli()
	require.NoError(t, k.SetPrice(ctx, oracletypes.OraclePrice{
		MarketIndex: 2, IndexPrice: 42, MarkPrice: 42,
		LastUpdatedTimestamp: blockTs,
	}))
	p, err := k.GetPrice(ctx, 2)
	require.NoError(t, err)
	require.EqualValues(t, 42, p.MarkPrice)
}
