package keeper_test

import (
	"context"
	"os"
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
	liqkeeper "github.com/perpdex/perpdex-l1/x/liquidation/keeper"
	liqtypes "github.com/perpdex/perpdex-l1/x/liquidation/types"
	markettypes "github.com/perpdex/perpdex-l1/x/market/types"
	tradekeeper "github.com/perpdex/perpdex-l1/x/trade/keeper"
)

func TestMain(m *testing.M) {
	cfg := sdk.GetConfig()
	cfg.SetBech32PrefixForAccount("px", "pxpub")
	os.Exit(m.Run())
}

type stubAccount struct {
	accounts map[uint64]accounttypes.Account
	pos      map[[2]uint64]accounttypes.AccountPosition
}

func newStubAccount() *stubAccount {
	return &stubAccount{
		accounts: map[uint64]accounttypes.Account{},
		pos:      map[[2]uint64]accounttypes.AccountPosition{},
	}
}

func (s *stubAccount) GetAccount(_ context.Context, idx uint64) (accounttypes.Account, error) {
	if a, ok := s.accounts[idx]; ok {
		return a, nil
	}
	return accounttypes.Account{}, accounttypes.ErrAccountNotFound.Wrapf("idx=%d", idx)
}
func (s *stubAccount) GetMasterAccountByOwner(_ context.Context, owner string) (accounttypes.Account, error) {
	for _, a := range s.accounts {
		if a.OwnerAddress == owner && a.AccountType == perptypes.MasterAccountType {
			return a, nil
		}
	}
	return accounttypes.Account{}, accounttypes.ErrAccountNotFound
}
func (s *stubAccount) GetPosition(_ context.Context, acc uint64, mkt uint32) (accounttypes.AccountPosition, error) {
	if p, ok := s.pos[[2]uint64{acc, uint64(mkt)}]; ok {
		return p, nil
	}
	return accounttypes.AccountPosition{
		AccountIndex: acc, MarketIndex: mkt,
		Position: math.ZeroInt(), EntryQuote: math.ZeroInt(),
		LastFundingRatePrefixSum: math.ZeroInt(), AllocatedMargin: math.ZeroInt(),
	}, nil
}
func (s *stubAccount) SetPosition(_ context.Context, p accounttypes.AccountPosition) error {
	s.pos[[2]uint64{p.AccountIndex, uint64(p.MarketIndex)}] = p
	return nil
}
func (s *stubAccount) AddCollateral(_ context.Context, _ uint64, _ math.Int) error { return nil }
func (s *stubAccount) IterateAccounts(_ context.Context, _ func(accounttypes.Account) bool) error {
	return nil
}
func (s *stubAccount) IsAuthorized(_ context.Context, signer string, idx uint64) (bool, error) {
	if a, ok := s.accounts[idx]; ok {
		return a.OwnerAddress == signer, nil
	}
	return false, nil
}

type stubMarket struct{}

func (stubMarket) GetMarket(_ context.Context, idx uint32) (markettypes.Market, error) {
	return markettypes.Market{MarketIndex: idx, MarketType: perptypes.MarketTypePerps}, nil
}
func (stubMarket) GetMarketDetails(_ context.Context, idx uint32) (markettypes.MarketDetails, error) {
	return markettypes.MarketDetails{MarketIndex: idx}, nil
}

type stubRisk struct {
	status uint32
}

func (s *stubRisk) GetHealthStatus(_ context.Context, _ uint64) (uint32, error) {
	return s.status, nil
}
func (s *stubRisk) GetPositionZeroPrice(_ context.Context, _ uint64, _ uint32) (uint32, error) {
	return 10, nil
}
func (s *stubRisk) GetPositionMarkValue(_ context.Context, _ uint64, _ uint32) (math.Int, error) {
	return math.ZeroInt(), nil
}
func (s *stubRisk) GetPositionUnrealizedPnL(_ context.Context, _ uint64, _ uint32) (math.Int, error) {
	return math.ZeroInt(), nil
}

type stubTrade struct{ calls int }

func (s *stubTrade) ApplyPerpsMatching(_ context.Context, _ tradekeeper.Fill) error {
	s.calls++
	return nil
}

func newKeeper(t *testing.T, ak *stubAccount, rk *stubRisk, tk *stubTrade) (liqkeeper.Keeper, sdk.Context) {
	t.Helper()
	keys := storetypes.NewKVStoreKeys(liqtypes.StoreKey)
	cdc := moduletestutil.MakeTestEncodingConfig().Codec
	cms := integration.CreateMultiStore(keys, log.NewTestLogger(t))
	ctx := sdk.NewContext(cms, cmtprototypes.Header{}, true, log.NewTestLogger(t))
	k := liqkeeper.NewKeeper(
		cdc,
		runtime.NewKVStoreService(keys[liqtypes.StoreKey]),
		"px1qv9pzxqlyckngw6zf9g9whn9d3eh4qvgsxc8cx",
		ak,
		stubMarket{},
		rk,
		tk,
	)
	return k, ctx
}

// TestLiquidate_BaseAmountCappedByPosition ensures that passing a
// base_amount greater than the victim's position is rejected rather than
// silently flipping the position (audit Blocker liquidation-2).
func TestLiquidate_BaseAmountCappedByPosition(t *testing.T) {
	ak := newStubAccount()
	ak.accounts[100] = accounttypes.Account{AccountIndex: 100, Collateral: math.ZeroInt()}
	ak.accounts[200] = accounttypes.Account{AccountIndex: 200, Collateral: math.ZeroInt()}
	ak.pos[[2]uint64{100, 0}] = accounttypes.AccountPosition{
		AccountIndex: 100, MarketIndex: 0,
		Position: math.NewInt(5), EntryQuote: math.NewInt(-50),
		LastFundingRatePrefixSum: math.ZeroInt(), AllocatedMargin: math.ZeroInt(),
	}
	rk := &stubRisk{status: perptypes.HealthPartialLiquidation}
	tk := &stubTrade{}
	k, ctx := newKeeper(t, ak, rk, tk)

	err := k.Liquidate(ctx, 100, 0, 999 /* > victim size */, 200)
	require.Error(t, err)
	require.ErrorIs(t, err, liqtypes.ErrInvalidParams)
	require.Equal(t, 0, tk.calls)
}

// TestLiquidate_ZeroBaseRejected checks the audit fix that rejects
// base_amount == 0 explicitly.
func TestLiquidate_ZeroBaseRejected(t *testing.T) {
	ak := newStubAccount()
	ak.accounts[100] = accounttypes.Account{AccountIndex: 100, Collateral: math.ZeroInt()}
	ak.pos[[2]uint64{100, 0}] = accounttypes.AccountPosition{
		AccountIndex: 100, MarketIndex: 0, Position: math.NewInt(3),
		EntryQuote: math.NewInt(-30), LastFundingRatePrefixSum: math.ZeroInt(),
		AllocatedMargin: math.ZeroInt(),
	}
	rk := &stubRisk{status: perptypes.HealthFullLiquidation}
	tk := &stubTrade{}
	k, ctx := newKeeper(t, ak, rk, tk)

	err := k.Liquidate(ctx, 100, 0, 0, 200)
	require.ErrorIs(t, err, liqtypes.ErrInvalidParams)
}

// TestDeleverage_RejectsUnauthorizedUserSender verifies that an ADL sender
// can only drive a deleverager account they own (audit msg server fix).
func TestDeleverage_RejectsUnauthorizedUserSender(t *testing.T) {
	ak := newStubAccount()
	authority := "px1qv9pzxqlyckngw6zf9g9whn9d3eh4qvgsxc8cx"
	owner := "px1zqt3uffvxvayzjz02ewkg6mj0xqg0r5463hcq9"
	outsider := "px1r5jzkv3egpr5u42uvd48z7rls6xefxazenrll9"

	ak.accounts[50] = accounttypes.Account{
		AccountIndex: 50, OwnerAddress: owner,
		AccountType: perptypes.MasterAccountType, Collateral: math.ZeroInt(),
	}
	rk := &stubRisk{status: perptypes.HealthFullLiquidation}
	tk := &stubTrade{}
	k, ctx := newKeeper(t, ak, rk, tk)
	srv := liqkeeper.NewMsgServerImpl(k)

	_, err := srv.Deleverage(ctx, &liqtypes.MsgDeleverage{
		Sender:                  outsider,
		VictimAccountIndex:      10,
		MarketIndex:             0,
		DeleveragerAccountIndex: 50,
		BaseAmount:              1,
	})
	require.ErrorIs(t, err, liqtypes.ErrUnauthorized)
	_ = authority
}
