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
	tradekeeper "github.com/perpdex/perpdex-l1/x/trade/keeper"
	tradetypes "github.com/perpdex/perpdex-l1/x/trade/types"
)

// --- in-memory stub keepers ---------------------------------------------

type stubAccount struct {
	accounts map[uint64]*accounttypes.Account
	pos      map[[2]uint64]*accounttypes.AccountPosition
	assets   map[[2]uint64]*accounttypes.AccountAsset
}

func newStubAccount() *stubAccount {
	return &stubAccount{
		accounts: map[uint64]*accounttypes.Account{},
		pos:      map[[2]uint64]*accounttypes.AccountPosition{},
		assets:   map[[2]uint64]*accounttypes.AccountAsset{},
	}
}

func (s *stubAccount) GetAccount(_ context.Context, idx uint64) (accounttypes.Account, error) {
	if a, ok := s.accounts[idx]; ok {
		return *a, nil
	}
	return accounttypes.Account{AccountIndex: idx, Collateral: math.ZeroInt()}, nil
}

func (s *stubAccount) SetAccount(_ context.Context, a accounttypes.Account) error {
	cp := a
	s.accounts[a.AccountIndex] = &cp
	return nil
}

func posKey(acc uint64, mkt uint32) [2]uint64 { return [2]uint64{acc, uint64(mkt)} }

func (s *stubAccount) GetPosition(_ context.Context, acc uint64, mkt uint32) (accounttypes.AccountPosition, error) {
	if p, ok := s.pos[posKey(acc, mkt)]; ok {
		return *p, nil
	}
	return accounttypes.AccountPosition{
		AccountIndex:             acc,
		MarketIndex:              mkt,
		Position:                 math.ZeroInt(),
		EntryQuote:               math.ZeroInt(),
		LastFundingRatePrefixSum: math.ZeroInt(),
		AllocatedMargin:          math.ZeroInt(),
		MarginMode:               perptypes.CrossMargin,
	}, nil
}

func (s *stubAccount) SetPosition(_ context.Context, p accounttypes.AccountPosition) error {
	cp := p
	s.pos[posKey(p.AccountIndex, p.MarketIndex)] = &cp
	return nil
}

func (s *stubAccount) AddCollateral(_ context.Context, idx uint64, delta math.Int) error {
	a := s.accounts[idx]
	if a == nil {
		a = &accounttypes.Account{AccountIndex: idx, Collateral: math.ZeroInt()}
		s.accounts[idx] = a
	}
	if a.Collateral.IsNil() {
		a.Collateral = math.ZeroInt()
	}
	a.Collateral = a.Collateral.Add(delta)
	return nil
}

func (s *stubAccount) GetAccountAsset(_ context.Context, acc uint64, assetIdx uint32) (accounttypes.AccountAsset, error) {
	if a, ok := s.assets[posKey(acc, assetIdx)]; ok {
		return *a, nil
	}
	return accounttypes.AccountAsset{
		AccountIndex:  acc,
		AssetIndex:    assetIdx,
		Balance:       math.ZeroInt(),
		LockedBalance: math.ZeroInt(),
	}, nil
}

func (s *stubAccount) SetAccountAsset(_ context.Context, aa accounttypes.AccountAsset) error {
	cp := aa
	s.assets[posKey(aa.AccountIndex, aa.AssetIndex)] = &cp
	return nil
}

type stubMarket struct {
	oi map[uint32]int64
}

func (s *stubMarket) GetMarket(_ context.Context, idx uint32) (markettypes.Market, error) {
	return markettypes.Market{MarketIndex: idx, MarketType: perptypes.MarketTypePerps}, nil
}
func (s *stubMarket) GetMarketDetails(_ context.Context, idx uint32) (markettypes.MarketDetails, error) {
	return markettypes.MarketDetails{MarketIndex: idx}, nil
}
func (s *stubMarket) UpdateOpenInterest(_ context.Context, idx uint32, delta int64) error {
	if s.oi == nil {
		s.oi = map[uint32]int64{}
	}
	s.oi[idx] += delta
	return nil
}

type stubFunding struct{}

func (stubFunding) SettlePositionFunding(_ context.Context, _ uint64, _ uint32) error { return nil }

type stubRisk struct {
	reject       bool
	snapshots    int
	riskChecks   int
	rejectOnCall int // >0 means reject only on Nth riskCheck call

	// Knobs for the lighter-aligned isolated margin / cross collateral
	// pre-checks. Tests that need to exercise the
	// `ErrMaker/TakerInsufficientCollateral` branches can set
	// `availableCollateral` per-account; the default behaviour
	// reports an essentially unbounded headroom so existing tests
	// keep passing.
	availableCollateral map[uint64]math.Int
	// markPrice is consulted by ComputePositionInitialMargin /
	// ComputeUnrealizedPnLAt. Default of 0 means the margin auto-
	// allocation logic short-circuits to zero (no IM, no uPnL),
	// preserving legacy test expectations.
	markPrice uint64
	// imfBps controls the synthetic IM = notional * imfBps / 10_000
	// computed by the stub.
	imfBps uint64
}

func (s *stubRisk) IsValidRiskChange(_ context.Context, _ uint64) (bool, error) {
	s.riskChecks++
	if s.rejectOnCall > 0 && s.riskChecks == s.rejectOnCall {
		return false, nil
	}
	return !s.reject, nil
}
func (s *stubRisk) SnapshotPreRisk(_ context.Context, _ uint64) error {
	s.snapshots++
	return nil
}

func (s *stubRisk) GetAvailableUsdcCollateral(_ context.Context, accountIdx uint64) (math.Int, error) {
	if s.availableCollateral != nil {
		if v, ok := s.availableCollateral[accountIdx]; ok {
			return v, nil
		}
	}
	// Default: large constant so non-isolated / no-margin-delta tests
	// don't trip the cross-collateral pre-check.
	return math.NewIntFromUint64(1<<62 - 1), nil
}

func (s *stubRisk) ComputePositionInitialMargin(_ context.Context, _ uint32, posAbs math.Int) (math.Int, error) {
	if s.markPrice == 0 || s.imfBps == 0 || posAbs.IsNil() || posAbs.IsZero() {
		return math.ZeroInt(), nil
	}
	notional := posAbs.Mul(math.NewIntFromUint64(s.markPrice))
	return notional.Mul(math.NewIntFromUint64(s.imfBps)).Quo(math.NewInt(10_000)), nil
}

func (s *stubRisk) ComputeUnrealizedPnLAt(_ context.Context, _ uint32, position, entryQuote math.Int) (math.Int, error) {
	if s.markPrice == 0 || position.IsNil() || position.IsZero() {
		return math.ZeroInt(), nil
	}
	if entryQuote.IsNil() {
		entryQuote = math.ZeroInt()
	}
	return position.Mul(math.NewIntFromUint64(s.markPrice)).Sub(entryQuote), nil
}

func newSdkCtx(t *testing.T) (sdk.Context, *stubAccount, *stubMarket, *stubRisk, tradekeeper.Keeper) {
	t.Helper()
	keys := storetypes.NewKVStoreKeys(tradetypes.StoreKey)
	cdc := moduletestutil.MakeTestEncodingConfig().Codec
	cms := integration.CreateMultiStore(keys, log.NewTestLogger(t))
	ctx := sdk.NewContext(cms, cmtprototypes.Header{}, true, log.NewTestLogger(t))
	ak := newStubAccount()
	mk := &stubMarket{}
	rk := &stubRisk{}
	k := tradekeeper.NewKeeper(
		cdc,
		runtime.NewKVStoreService(keys[tradetypes.StoreKey]),
		"auth",
		ak,
		mk,
		stubFunding{},
		rk,
	)
	return ctx, ak, mk, rk, k
}

// TestApplyPerpsMatching_OIRoundTrip verifies that opening and then closing
// a position nets open interest back to 0 (audit High trade-4).
func TestApplyPerpsMatching_OIRoundTrip(t *testing.T) {
	ctx, ak, mk, rk, k := newSdkCtx(t)
	_ = rk

	// Seed some collateral so fee deduction never tips either account into
	// the negative (risk keeper will accept because stub returns true).
	require.NoError(t, ak.SetAccount(ctx, accounttypes.Account{
		AccountIndex: 10, Collateral: math.NewInt(1_000_000),
	}))
	require.NoError(t, ak.SetAccount(ctx, accounttypes.Account{
		AccountIndex: 20, Collateral: math.NewInt(1_000_000),
	}))

	require.NoError(t, k.ApplyPerpsMatching(ctx, tradekeeper.Fill{
		MakerAccountIndex: 10, TakerAccountIndex: 20,
		MarketIndex: 1, Price: 100, BaseAmount: 7,
		IsTakerAsk: false, NoFee: true,
	}))
	require.Equal(t, int64(7), mk.oi[1])

	// Taker now closes against the same maker; OI must return to zero.
	require.NoError(t, k.ApplyPerpsMatching(ctx, tradekeeper.Fill{
		MakerAccountIndex: 10, TakerAccountIndex: 20,
		MarketIndex: 1, Price: 100, BaseAmount: 7,
		IsTakerAsk: true, NoFee: true,
	}))
	require.Equal(t, int64(0), mk.oi[1])
}

// TestApplyPerpsMatching_RejectsMakerRisk ensures the maker side is still
// risk-checked (not only taker) — audit Blocker trade-1.
func TestApplyPerpsMatching_RejectsMakerRisk(t *testing.T) {
	ctx, ak, _, rk, k := newSdkCtx(t)
	rk.rejectOnCall = 2 // first call = taker, second = maker

	require.NoError(t, ak.SetAccount(ctx, accounttypes.Account{AccountIndex: 10, Collateral: math.NewInt(1)}))
	require.NoError(t, ak.SetAccount(ctx, accounttypes.Account{AccountIndex: 20, Collateral: math.NewInt(1)}))

	err := k.ApplyPerpsMatching(ctx, tradekeeper.Fill{
		MakerAccountIndex: 10, TakerAccountIndex: 20,
		MarketIndex: 1, Price: 10, BaseAmount: 1,
		IsTakerAsk: false, NoFee: true,
	})
	require.Error(t, err)
	require.Contains(t, err.Error(), "risk regression")
	require.Equal(t, 2, rk.snapshots) // maker + taker both snapshotted
}

// TestApplyPerpsMatching_LiquidationFeeRoutesToLLP exercises the new
// improvement-over-zero-price liquidation fee path. When the fill price
// is BETTER than the zero price by `improvement_per_unit`, the
// computed fee is debited from the maker (victim) and credited to
// LiquidationFeeRecipient (LLP / Insurance Fund). Standard
// taker/maker fees do not apply on the same fill (caller sets them to
// 0), so the treasury stays untouched.
func TestApplyPerpsMatching_LiquidationFeeRoutesToLLP(t *testing.T) {
	ctx, ak, _, _, k := newSdkCtx(t)
	const (
		victimIdx = uint64(100)
		takerIdx  = uint64(200)
		llpIdx    = perptypes.InsuranceFundOperatorAccountIdx
	)
	require.NoError(t, ak.SetAccount(ctx, accounttypes.Account{
		AccountIndex: victimIdx, Collateral: math.NewInt(10_000_000),
	}))
	require.NoError(t, ak.SetAccount(ctx, accounttypes.Account{
		AccountIndex: takerIdx, Collateral: math.NewInt(10_000_000),
	}))
	require.NoError(t, ak.SetAccount(ctx, accounttypes.Account{
		AccountIndex: llpIdx, Collateral: math.NewInt(0),
	}))
	// Victim is long; the fill closes via taker-ask (sells base).
	// improvement = (Price - ZeroPrice) * BaseAmount = (110-100)*1000 = 10_000.
	// liq_fee_bps = 10_000 / FeeTick = 1% on improvement = 100.
	// 1% notional cap = 110*1000/100 = 1100. raw_fee=100 < 1100 ⇒ fee=100.
	require.NoError(t, k.ApplyPerpsMatching(ctx, tradekeeper.Fill{
		MakerAccountIndex:       victimIdx,
		TakerAccountIndex:       takerIdx,
		MarketIndex:             1,
		Price:                   110,
		BaseAmount:              1000,
		IsTakerAsk:              true, // taker sells, victim closes long
		ZeroPrice:               100,
		LiquidationFeeBps:       10_000, // 1% in fee tick units
		LiquidationFeeRecipient: llpIdx,
		NoFee:                   false,
		SkipMakerRiskCheck:      true,
	}))
	llp, err := ak.GetAccount(ctx, llpIdx)
	require.NoError(t, err)
	require.Equal(t, "100", llp.Collateral.String(),
		"LLP must receive 1%% of improvement, NOT the treasury")
	treasury, err := ak.GetAccount(ctx, perptypes.TreasuryAccountIndex)
	require.NoError(t, err)
	require.True(t, treasury.Collateral.IsZero(),
		"Treasury must remain untouched on a fee-less liquidation fill")
}

// TestApplyPerpsMatching_LiquidationFeeNoneAtZeroPrice asserts the
// edge case the partial-liquidation engine relies on: when the fill
// price equals the zero price (the keeper-driven IoC close-out path),
// the improvement is zero and no fee is charged.
func TestApplyPerpsMatching_LiquidationFeeNoneAtZeroPrice(t *testing.T) {
	ctx, ak, _, _, k := newSdkCtx(t)
	const llpIdx = perptypes.InsuranceFundOperatorAccountIdx
	require.NoError(t, ak.SetAccount(ctx, accounttypes.Account{
		AccountIndex: 100, Collateral: math.NewInt(1_000_000),
	}))
	require.NoError(t, ak.SetAccount(ctx, accounttypes.Account{
		AccountIndex: 200, Collateral: math.NewInt(1_000_000),
	}))
	require.NoError(t, ak.SetAccount(ctx, accounttypes.Account{
		AccountIndex: llpIdx, Collateral: math.NewInt(0),
	}))
	require.NoError(t, k.ApplyPerpsMatching(ctx, tradekeeper.Fill{
		MakerAccountIndex:       100,
		TakerAccountIndex:       200,
		MarketIndex:             1,
		Price:                   100,
		BaseAmount:              1000,
		IsTakerAsk:              true,
		ZeroPrice:               100, // fill == zero price
		LiquidationFeeBps:       10_000,
		LiquidationFeeRecipient: llpIdx,
		NoFee:                   false,
		SkipMakerRiskCheck:      true,
	}))
	llp, err := ak.GetAccount(ctx, llpIdx)
	require.NoError(t, err)
	require.True(t, llp.Collateral.IsZero(),
		"no improvement ⇒ no fee; LLP must not receive collateral")
}

// TestApplySpotMatching_RejectsNegativeBalance ensures a buy-side spot trade
// against a zero-balance maker errors instead of writing a negative balance
// (audit High trade-8).
func TestApplySpotMatching_RejectsNegativeBalance(t *testing.T) {
	ctx, _, _, _, k := newSdkCtx(t)

	// Taker buys from maker, but maker has no base balance.
	err := k.ApplySpotMatching(ctx, tradekeeper.Fill{
		MakerAccountIndex: 100, TakerAccountIndex: 200,
		MarketIndex: 2, Price: 5, BaseAmount: 10,
		IsTakerAsk: false, NoFee: true,
	}, uint32(1), uint32(3))
	require.Error(t, err)
	require.Contains(t, err.Error(), "insufficient balance")
}
