package keeper_test

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"

	cmtprototypes "github.com/cometbft/cometbft/proto/tendermint/types"

	"cosmossdk.io/log"
	"cosmossdk.io/math"
	storetypes "cosmossdk.io/store/types"

	"github.com/cosmos/cosmos-sdk/runtime"
	"github.com/cosmos/cosmos-sdk/testutil/integration"
	sdk "github.com/cosmos/cosmos-sdk/types"
	moduletestutil "github.com/cosmos/cosmos-sdk/types/module/testutil"

	perptypes "github.com/perpdex/perpdex-l1/types"
	accounttypes "github.com/perpdex/perpdex-l1/x/account/types"
	markettypes "github.com/perpdex/perpdex-l1/x/market/types"
	oracletypes "github.com/perpdex/perpdex-l1/x/oracle/types"
	riskkeeper "github.com/perpdex/perpdex-l1/x/risk/keeper"
	risktypes "github.com/perpdex/perpdex-l1/x/risk/types"
)

type stubAccountKeeper struct {
	acc accounttypes.Account
	pos accounttypes.AccountPosition
}

func (s stubAccountKeeper) GetAccount(_ context.Context, _ uint64) (accounttypes.Account, error) {
	return s.acc, nil
}
func (s stubAccountKeeper) GetPosition(_ context.Context, _ uint64, mkt uint32) (accounttypes.AccountPosition, error) {
	if mkt == s.pos.MarketIndex {
		return s.pos, nil
	}
	return accounttypes.AccountPosition{
		Position: math.ZeroInt(), EntryQuote: math.ZeroInt(),
		LastFundingRatePrefixSum: math.ZeroInt(), AllocatedMargin: math.ZeroInt(),
	}, nil
}
func (s stubAccountKeeper) GetAccountAsset(_ context.Context, acc uint64, assetIdx uint32) (accounttypes.AccountAsset, error) {
	return accounttypes.AccountAsset{
		AccountIndex: acc, AssetIndex: assetIdx,
		Balance: math.ZeroInt(), LockedBalance: math.ZeroInt(),
	}, nil
}
func (s stubAccountKeeper) IterateAccounts(_ context.Context, _ func(accounttypes.Account) bool) error {
	return nil
}

type stubMarketKeeper struct{ md markettypes.MarketDetails }

func (s stubMarketKeeper) GetMarket(_ context.Context, idx uint32) (markettypes.Market, error) {
	return markettypes.Market{MarketIndex: idx, MarketType: perptypes.MarketTypePerps}, nil
}
func (s stubMarketKeeper) GetMarketDetails(_ context.Context, _ uint32) (markettypes.MarketDetails, error) {
	return s.md, nil
}

type stubOracleKeeper struct {
	price oracletypes.OraclePrice
	err   error
}

func (s stubOracleKeeper) GetPrice(_ context.Context, _ uint32) (oracletypes.OraclePrice, error) {
	return s.price, s.err
}
func (s stubOracleKeeper) GetFreshPrice(_ context.Context, _ uint32) (oracletypes.OraclePrice, error) {
	return s.price, s.err
}

func makeKeeper(t *testing.T, ak stubAccountKeeper, mk stubMarketKeeper, ok stubOracleKeeper) (riskkeeper.Keeper, sdk.Context) {
	t.Helper()
	keys := storetypes.NewKVStoreKeys(risktypes.StoreKey)
	cdc := moduletestutil.MakeTestEncodingConfig().Codec
	cms := integration.CreateMultiStore(keys, log.NewTestLogger(t))
	ctx := sdk.NewContext(cms, cmtprototypes.Header{}, true, log.NewTestLogger(t))
	return riskkeeper.NewKeeper(
		cdc,
		runtime.NewKVStoreService(keys[risktypes.StoreKey]),
		"auth",
		ak, mk, ok,
	), ctx
}

// TestComputeRiskInfo_StalePriceFailsClosed ensures that non-zero positions
// with a missing or stale oracle price surface an explicit error rather than
// being silently skipped (audit Blocker risk-3).
func TestComputeRiskInfo_StalePriceFailsClosed(t *testing.T) {
	ak := stubAccountKeeper{
		acc: accounttypes.Account{AccountIndex: 1, Collateral: math.NewInt(1000)},
		pos: accounttypes.AccountPosition{
			AccountIndex: 1, MarketIndex: 0,
			Position: math.NewInt(5), EntryQuote: math.NewInt(500),
			LastFundingRatePrefixSum: math.ZeroInt(), AllocatedMargin: math.ZeroInt(),
		},
	}
	mk := stubMarketKeeper{md: markettypes.MarketDetails{}}
	ok := stubOracleKeeper{err: oracletypes.ErrStalePrice}
	k, ctx := makeKeeper(t, ak, mk, ok)

	_, err := k.ComputeRiskInfo(ctx, 1)
	require.ErrorIs(t, err, risktypes.ErrMissingPrice)
}

// TestComputeRiskInfo_ZeroMarkPriceRejected enforces the "non-zero mark"
// invariant on non-zero positions.
func TestComputeRiskInfo_ZeroMarkPriceRejected(t *testing.T) {
	ak := stubAccountKeeper{
		acc: accounttypes.Account{AccountIndex: 1, Collateral: math.NewInt(1000)},
		pos: accounttypes.AccountPosition{
			AccountIndex: 1, MarketIndex: 0,
			Position: math.NewInt(5), EntryQuote: math.NewInt(500),
			LastFundingRatePrefixSum: math.ZeroInt(), AllocatedMargin: math.ZeroInt(),
		},
	}
	mk := stubMarketKeeper{md: markettypes.MarketDetails{}}
	ok := stubOracleKeeper{price: oracletypes.OraclePrice{MarkPrice: 0, IndexPrice: 100}}
	k, ctx := makeKeeper(t, ak, mk, ok)

	_, err := k.ComputeRiskInfo(ctx, 1)
	require.ErrorIs(t, err, risktypes.ErrZeroMarkPrice)
}

// TestIsValidRiskChange_NoPreStateFailClosed verifies that an unhealthy post
// state without a pre-state snapshot fails closed (audit Blocker risk-3).
func TestIsValidRiskChange_NoPreStateFailClosed(t *testing.T) {
	// Position bought at 100_000 but mark is 10_000, so the account is
	// deeply under water → BANKRUPTCY in the post-state.
	ak := stubAccountKeeper{
		acc: accounttypes.Account{AccountIndex: 1, Collateral: math.NewInt(10)},
		pos: accounttypes.AccountPosition{
			AccountIndex: 1, MarketIndex: 0,
			Position: math.NewInt(1_000_000), EntryQuote: math.NewInt(100_000_000_000),
			LastFundingRatePrefixSum: math.ZeroInt(), AllocatedMargin: math.ZeroInt(),
		},
	}
	mk := stubMarketKeeper{md: markettypes.MarketDetails{
		DefaultInitialMarginFraction: 1000, MaintenanceMarginFraction: 500,
		CloseOutMarginFraction: 250,
	}}
	ok := stubOracleKeeper{price: oracletypes.OraclePrice{MarkPrice: 10_000}}
	k, ctx := makeKeeper(t, ak, mk, ok)

	ok2, err := k.IsValidRiskChange(ctx, 1)
	require.NoError(t, err)
	require.False(t, ok2)
}

// TestGetPositionMarkValue_ZeroForEmptyPosition is a sanity check: empty
// positions should not require a live oracle.
func TestGetPositionMarkValue_ZeroForEmptyPosition(t *testing.T) {
	ak := stubAccountKeeper{
		acc: accounttypes.Account{AccountIndex: 1, Collateral: math.ZeroInt()},
		pos: accounttypes.AccountPosition{
			AccountIndex: 1, MarketIndex: 0,
			Position:                 math.ZeroInt(),
			EntryQuote:               math.ZeroInt(),
			LastFundingRatePrefixSum: math.ZeroInt(),
			AllocatedMargin:          math.ZeroInt(),
		},
	}
	ok := stubOracleKeeper{err: oracletypes.ErrStalePrice}
	k, ctx := makeKeeper(t, ak, stubMarketKeeper{}, ok)

	v, err := k.GetPositionMarkValue(ctx, 1, 0)
	require.NoError(t, err)
	require.True(t, v.IsZero())
}
