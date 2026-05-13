// Package tests hosts the external (`package tests`) test suite for
// the x/risk keeper. The suite is split by business domain — cross
// margin health, isolated margin health, liquidation / zero-price and
// risk-change validation — and shares the keeper / account / market
// stubs declared in this file.
package tests

import (
	"context"
	"testing"

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
	riskkeeper "github.com/perpdex/perpdex-l1/x/risk/keeper"
	risktypes "github.com/perpdex/perpdex-l1/x/risk/types"
)

type stubAccountKeeper struct {
	acc accounttypes.Account
	pos accounttypes.AccountPosition
}

// Pointer receivers so the test can mutate `acc`/`pos` AFTER
// constructing the keeper (e.g. for pre/post-state
// IsValidRiskChangeFrom scenarios) without rebuilding the whole
// keeper instance.
func (s *stubAccountKeeper) GetAccount(_ context.Context, _ uint64) (accounttypes.Account, error) {
	return s.acc, nil
}
func (s *stubAccountKeeper) GetPosition(_ context.Context, _ uint64, mkt uint32) (accounttypes.AccountPosition, error) {
	if mkt == s.pos.MarketIndex {
		return s.pos, nil
	}
	return accounttypes.AccountPosition{
		BaseSize: math.ZeroInt(), EntryQuote: math.ZeroInt(),
		LastFundingRatePrefixSum: math.ZeroInt(), AllocatedMargin: math.ZeroInt(),
	}, nil
}
func (s *stubAccountKeeper) GetAccountAsset(_ context.Context, acc uint64, assetIdx uint32) (accounttypes.AccountAsset, error) {
	return accounttypes.AccountAsset{
		AccountIndex: acc, AssetIndex: assetIdx,
		Balance: math.ZeroInt(), LockedBalance: math.ZeroInt(),
	}, nil
}
func (s *stubAccountKeeper) IterateAccounts(_ context.Context, _ func(accounttypes.Account) bool) error {
	return nil
}

// IterateAccountPositions yields s.pos when the seeded account matches.
// Empty positions are skipped to mirror the real keeper.
func (s *stubAccountKeeper) IterateAccountPositions(
	_ context.Context,
	accountIdx uint64,
	cb func(accounttypes.AccountPosition) bool,
) error {
	if s.pos.AccountIndex != accountIdx {
		return nil
	}
	if s.pos.BaseSize.IsNil() || s.pos.BaseSize.IsZero() {
		return nil
	}
	cb(s.pos)
	return nil
}

// stubMarketKeeper implements the gated mark reader locally so risk
// tests can exercise the same zero / staleness invariants as the live
// market keeper. `maxStaleness` defaults to 0 (gate disabled); tests
// that exercise the staleness gate set it together with
// `md.LastMarkPriceRefreshTimestamp`.
type stubMarketKeeper struct {
	md           markettypes.MarketDetails
	maxStaleness int64
}

func (s stubMarketKeeper) GetMarket(_ context.Context, idx uint32) (markettypes.Market, error) {
	return markettypes.Market{MarketIndex: idx, MarketType: perptypes.MarketTypePerps}, nil
}
func (s stubMarketKeeper) GetMarketDetails(_ context.Context, _ uint32) (markettypes.MarketDetails, error) {
	return s.md, nil
}

func (s stubMarketKeeper) gateMark(ctx context.Context, marketIdx uint32, d markettypes.MarketDetails) error {
	if d.MarkPrice == 0 {
		return markettypes.ErrZeroMarkPrice.Wrapf("market=%d", marketIdx)
	}
	if s.maxStaleness <= 0 {
		return nil
	}
	now := sdk.UnwrapSDKContext(ctx).BlockTime().UnixMilli()
	if d.LastMarkPriceRefreshTimestamp == 0 || now-d.LastMarkPriceRefreshTimestamp > s.maxStaleness {
		return markettypes.ErrStaleMarkPrice.Wrapf("market=%d", marketIdx)
	}
	return nil
}

func (s stubMarketKeeper) GetMarkPrice(ctx context.Context, marketIdx uint32) (uint32, error) {
	d, err := s.GetMarketDetails(ctx, marketIdx)
	if err != nil {
		return 0, err
	}
	if err := s.gateMark(ctx, marketIdx, d); err != nil {
		return 0, err
	}
	return d.MarkPrice, nil
}

func (s stubMarketKeeper) GetMarkPriceAndDetails(ctx context.Context, marketIdx uint32) (uint32, markettypes.MarketDetails, error) {
	d, err := s.GetMarketDetails(ctx, marketIdx)
	if err != nil {
		return 0, markettypes.MarketDetails{}, err
	}
	if err := s.gateMark(ctx, marketIdx, d); err != nil {
		return 0, markettypes.MarketDetails{}, err
	}
	return d.MarkPrice, d, nil
}

func makeKeeper(t *testing.T, ak *stubAccountKeeper, mk stubMarketKeeper) (riskkeeper.Keeper, sdk.Context) {
	t.Helper()
	keys := storetypes.NewKVStoreKeys(risktypes.StoreKey)
	cdc := moduletestutil.MakeTestEncodingConfig().Codec
	cms := integration.CreateMultiStore(keys, log.NewTestLogger(t))
	ctx := sdk.NewContext(cms, cmtprototypes.Header{}, true, log.NewTestLogger(t))
	return riskkeeper.NewKeeper(
		cdc,
		runtime.NewKVStoreService(keys[risktypes.StoreKey]),
		"auth",
		ak, mk,
	), ctx
}
