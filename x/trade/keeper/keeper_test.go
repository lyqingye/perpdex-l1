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
	risktypes "github.com/perpdex/perpdex-l1/x/risk/types"
	tradekeeper "github.com/perpdex/perpdex-l1/x/trade/keeper"
	tradetypes "github.com/perpdex/perpdex-l1/x/trade/types"
)

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

// SetAccount is a fixture-setup convenience kept on the stub for tests
// in this package. Production code never calls it: the AccountKeeper
// interface in x/trade/types does not expose a generic Account setter
// — production writes go through cohesive mutators.
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
		BaseSize:                 math.ZeroInt(),
		EntryQuote:               math.ZeroInt(),
		LastFundingRatePrefixSum: math.ZeroInt(),
		AllocatedMargin:          math.ZeroInt(),
		MarginMode:               perptypes.CrossMargin,
	}, nil
}

// SetPosition is the stub-only fixture helper (see SetAccount).
func (s *stubAccount) SetPosition(_ context.Context, p accounttypes.AccountPosition) error {
	cp := p
	s.pos[posKey(p.AccountIndex, p.MarketIndex)] = &cp
	return nil
}

// UpdatePosition mirrors the real keeper's RMW closure surface so
// the trade keeper (which now drives every position write through
// accountKeeper.UpdatePosition) can run against the stub.
func (s *stubAccount) UpdatePosition(
	ctx context.Context,
	accIdx uint64,
	marketIdx uint32,
	mut func(*accounttypes.AccountPosition) error,
) (accounttypes.AccountPosition, error) {
	pos, err := s.GetPosition(ctx, accIdx, marketIdx)
	if err != nil {
		return accounttypes.AccountPosition{}, err
	}
	pos.AccountIndex = accIdx
	pos.MarketIndex = marketIdx
	if err := mut(&pos); err != nil {
		return accounttypes.AccountPosition{}, err
	}
	if err := s.SetPosition(ctx, pos); err != nil {
		return accounttypes.AccountPosition{}, err
	}
	return pos, nil
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

// SetAccountAsset is the stub-only fixture helper (see SetAccount).
func (s *stubAccount) SetAccountAsset(_ context.Context, aa accounttypes.AccountAsset) error {
	cp := aa
	s.assets[posKey(aa.AccountIndex, aa.AssetIndex)] = &cp
	return nil
}

// TransferAccountAssetBalance mirrors the real keeper's spot transfer
// helper so the trade keeper's spotMakerDebit / spotTakerDebit paths
// run unchanged against the stub.
func (s *stubAccount) TransferAccountAssetBalance(
	ctx context.Context,
	from, to uint64,
	assetIdx uint32,
	amount math.Int,
	drainLockedFirst bool,
) error {
	if amount.IsNil() || amount.IsZero() {
		return nil
	}
	if amount.IsNegative() {
		return accounttypes.ErrInsufficientFunds.Wrap("transfer amount must be non-negative")
	}
	src, err := s.GetAccountAsset(ctx, from, assetIdx)
	if err != nil {
		return err
	}
	if src.Balance.IsNil() {
		src.Balance = math.ZeroInt()
	}
	if src.LockedBalance.IsNil() {
		src.LockedBalance = math.ZeroInt()
	}
	if drainLockedFirst {
		if src.Balance.LT(amount) {
			return accounttypes.ErrInsufficientFunds.Wrapf(
				"account %d asset %d have %s need %s",
				from, assetIdx, src.Balance.String(), amount.String())
		}
	} else {
		available := src.Balance.Sub(src.LockedBalance)
		if available.LT(amount) {
			return accounttypes.ErrInsufficientFunds.Wrapf(
				"account %d asset %d available %s need %s",
				from, assetIdx, available.String(), amount.String())
		}
	}
	dst, err := s.GetAccountAsset(ctx, to, assetIdx)
	if err != nil {
		return err
	}
	if dst.Balance.IsNil() {
		dst.Balance = math.ZeroInt()
	}
	if drainLockedFirst {
		drain := amount
		if drain.GT(src.LockedBalance) {
			drain = src.LockedBalance
		}
		src.LockedBalance = src.LockedBalance.Sub(drain)
	}
	src.Balance = src.Balance.Sub(amount)
	dst.Balance = dst.Balance.Add(amount)
	if err := s.SetAccountAsset(ctx, src); err != nil {
		return err
	}
	return s.SetAccountAsset(ctx, dst)
}

type stubMarket struct {
	oi map[uint32]int64
	// markPrice + imfBps populate the MarketDetails returned by
	// GetMarkPriceAndDetails; default 0 short-circuits the IM / uPnL
	// math, so tests that do not care about either field can ignore them.
	markPrice uint64
	imfBps    uint64
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

// GetMarkPriceAndDetails feeds the isolated-margin auto-allocation
// path. `markPrice` + `imfBps` from the surrounding test mutate the
// per-test return; defaults (zero) short-circuit the IM / uPnL math.
func (s *stubMarket) GetMarkPriceAndDetails(_ context.Context, idx uint32) (uint32, markettypes.MarketDetails, error) {
	md := markettypes.MarketDetails{
		MarketIndex:                  idx,
		DefaultInitialMarginFraction: uint32(s.imfBps),
	}
	return uint32(s.markPrice), md, nil
}

type stubFunding struct{}

func (stubFunding) SettlePositionFunding(_ context.Context, _ uint64, _ uint32) error { return nil }

type stubRisk struct {
	reject       bool
	snapshots    int
	riskChecks   int
	rejectOnCall int // >0 means reject only on Nth riskCheck call

	// Knobs for the isolated margin / cross collateral
	// pre-checks. Tests that need to exercise the
	// `ErrMaker/TakerInsufficientCollateral` branches can set
	// `availableCollateral` per-account; the default behaviour
	// reports an essentially unbounded headroom so existing tests
	// keep passing.
	availableCollateral map[uint64]math.Int
}

func (s *stubRisk) IsValidRiskChangeFrom(_ context.Context, _ uint64, _ risktypes.PreRiskSnapshot) (bool, error) {
	s.riskChecks++
	if s.rejectOnCall > 0 && s.riskChecks == s.rejectOnCall {
		return false, nil
	}
	return !s.reject, nil
}
func (s *stubRisk) SnapshotRisk(_ context.Context, _ uint64) (risktypes.PreRiskSnapshot, error) {
	s.snapshots++
	return risktypes.PreRiskSnapshot{}, nil
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

	require.NoError(t, k.ApplyPerpsMatching(ctx, tradekeeper.PerpFill{
		MakerAccountIndex: 10, TakerAccountIndex: 20,
		MarketIndex: 1, Price: 100, BaseAmount: 7,
		IsTakerAsk: false, NoFee: true,
	}))
	require.Equal(t, int64(7), mk.oi[1])

	// Taker now closes against the same maker; OI must return to zero.
	require.NoError(t, k.ApplyPerpsMatching(ctx, tradekeeper.PerpFill{
		MakerAccountIndex: 10, TakerAccountIndex: 20,
		MarketIndex: 1, Price: 100, BaseAmount: 7,
		IsTakerAsk: true, NoFee: true,
	}))
	require.Equal(t, int64(0), mk.oi[1])
}

// TestApplyPerpsMatching_RejectsMakerRisk pins the invariant that the
// maker side is risk-checked alongside the taker; failing the maker
// risk check rejects the whole match.
func TestApplyPerpsMatching_RejectsMakerRisk(t *testing.T) {
	ctx, ak, _, rk, k := newSdkCtx(t)
	rk.rejectOnCall = 2 // first call = taker, second = maker

	require.NoError(t, ak.SetAccount(ctx, accounttypes.Account{AccountIndex: 10, Collateral: math.NewInt(1)}))
	require.NoError(t, ak.SetAccount(ctx, accounttypes.Account{AccountIndex: 20, Collateral: math.NewInt(1)}))

	err := k.ApplyPerpsMatching(ctx, tradekeeper.PerpFill{
		MakerAccountIndex: 10, TakerAccountIndex: 20,
		MarketIndex: 1, Price: 10, BaseAmount: 1,
		IsTakerAsk: false, NoFee: true,
	})
	require.Error(t, err)
	require.Contains(t, err.Error(), "risk regression")
	require.Equal(t, 2, rk.snapshots) // maker + taker both snapshotted
}

// TestApplyPerpsMatching_LiquidationFeeRoutesToLLP exercises the
// improvement-over-zero-price liquidation fee path. When the fill
// price is strictly better than the zero price for the victim, an
// improvement fee is debited from the side being closed and credited
// to LiquidationFeeRecipient (LLP / Insurance Fund). Standard
// taker/maker fees do not apply on the same fill (caller sets them
// to 0), so the treasury stays untouched.
//
// Numerical expectations:
//
//	improvement     = (Price - ZeroPrice) * BaseAmount = (110-100)*1000 = 10_000
//	notional        = Price * BaseAmount               = 110*1000       = 110_000
//	price_diff_rate = (|Price-ZeroPrice| * FeeTick) / Price
//	                = (10 * 1_000_000) / 110 ≈ 90_909
//	effective_rate  = min(LiquidationFeeBps=10_000, 90_909) = 10_000
//	fee             = notional * 10_000 / FeeTick
//	                = 110_000 * 10_000 / 1_000_000        = 1_100
//
// The cap drops out of the `min(LiquidationFeeBps, price_diff_rate)`
// rate, scaled across notional rather than across improvement — the
// per-trade taker fee bound enforced in `matching_engine.rs`
// `_compute_liquidation_taker_fee`.
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
	require.NoError(t, k.ApplyPerpsMatching(ctx, tradekeeper.PerpFill{
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
	require.Equal(t, "1100", llp.Collateral.String(),
		"LLP must receive notional * min(bps, price_diff_rate) / FeeTick")
	treasury, err := ak.GetAccount(ctx, perptypes.TreasuryAccountIndex)
	require.NoError(t, err)
	require.True(t, treasury.Collateral.IsZero(),
		"Treasury must remain untouched on a fee-less liquidation fill")
}

// TestApplyPerpsMatching_LiquidationFeePriceDiffRateBound asserts the
// `min(LiquidationFeeBps, price_diff_rate)` ceiling: when the price
// improvement over the zero-price floor is small relative to the
// price (price_diff_rate < LiquidationFeeBps), the fee is bounded by
// price_diff_rate, NOT by the configured LiquidationFee. Without the
// rate bound, a tiny improvement at a high LiquidationFee would
// allow fees that exceed the actual gain.
//
// Setup: Price=101, ZeroPrice=100, BaseAmount=1000, fee_bps=50_000 (5%).
//
//	improvement     = 1 * 1000 = 1000
//	notional        = 101 * 1000 = 101_000
//	price_diff_rate = (1 * 1_000_000) / 101 ≈ 9_900
//	effective_rate  = min(50_000, 9_900) = 9_900
//	fee             = 101_000 * 9_900 / 1_000_000 = 999  (truncated)
//
// Without the price_diff_rate cap the fee would have been
// `notional * 50_000 / FeeTick = 5_050`, ~5x larger than the actual
// improvement — the bug the spec explicitly guards against.
func TestApplyPerpsMatching_LiquidationFeePriceDiffRateBound(t *testing.T) {
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
	require.NoError(t, k.ApplyPerpsMatching(ctx, tradekeeper.PerpFill{
		MakerAccountIndex:       victimIdx,
		TakerAccountIndex:       takerIdx,
		MarketIndex:             1,
		Price:                   101,
		BaseAmount:              1000,
		IsTakerAsk:              true,
		ZeroPrice:               100,
		LiquidationFeeBps:       50_000, // 5%, deliberately oversized
		LiquidationFeeRecipient: llpIdx,
		SkipMakerRiskCheck:      true,
	}))
	llp, err := ak.GetAccount(ctx, llpIdx)
	require.NoError(t, err)
	require.Equal(t, "999", llp.Collateral.String(),
		"price_diff_rate must clip the LLP fee below the bps ceiling")
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
	require.NoError(t, k.ApplyPerpsMatching(ctx, tradekeeper.PerpFill{
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

// TestApplyPerpsMatching_SkipTakerRiskCheck verifies the flag
// introduced for `Deleverage`'s LLP / IF path: when the taker is the
// IF/LLP absorber, both the pre-trade snapshot and the post-trade
// `IsValidRiskChangeFrom` on the taker must be skipped (the absorber's
// collateral sufficiency is gated by `tryLLPAbsorb`'s pre-trade
// `SimulateRiskAfterTakeover`/IMR check and the
// `is_*_has_enough_cross_collateral` assert instead). The maker side
// keeps its full risk pipeline.
func TestApplyPerpsMatching_SkipTakerRiskCheck(t *testing.T) {
	ctx, ak, _, rk, k := newSdkCtx(t)
	require.NoError(t, ak.SetAccount(ctx, accounttypes.Account{
		AccountIndex: 10, Collateral: math.NewInt(1_000_000),
	}))
	require.NoError(t, ak.SetAccount(ctx, accounttypes.Account{
		AccountIndex: 20, Collateral: math.NewInt(1_000_000),
	}))

	require.NoError(t, k.ApplyPerpsMatching(ctx, tradekeeper.PerpFill{
		MakerAccountIndex:  10,
		TakerAccountIndex:  20,
		MarketIndex:        1,
		Price:              100,
		BaseAmount:         5,
		IsTakerAsk:         true,
		NoFee:              true,
		SkipTakerRiskCheck: true,
	}))
	// Only the maker should be snapshotted and risk-checked once;
	// the taker side is fully bypassed.
	require.Equal(t, 1, rk.snapshots,
		"taker pre-snapshot must be skipped under SkipTakerRiskCheck")
	require.Equal(t, 1, rk.riskChecks,
		"only the maker should run IsValidRiskChangeFrom under SkipTakerRiskCheck")

	// SkipMakerRiskCheck stays independent: turning ON SkipMaker as
	// well drops the post-check count to 0 (we still snapshot
	// nothing on either side, and never enter the post-trade loop).
	ctx2, ak2, _, rk2, k2 := newSdkCtx(t)
	require.NoError(t, ak2.SetAccount(ctx2, accounttypes.Account{
		AccountIndex: 10, Collateral: math.NewInt(1_000_000),
	}))
	require.NoError(t, ak2.SetAccount(ctx2, accounttypes.Account{
		AccountIndex: 20, Collateral: math.NewInt(1_000_000),
	}))
	require.NoError(t, k2.ApplyPerpsMatching(ctx2, tradekeeper.PerpFill{
		MakerAccountIndex:  10,
		TakerAccountIndex:  20,
		MarketIndex:        1,
		Price:              100,
		BaseAmount:         5,
		IsTakerAsk:         true,
		NoFee:              true,
		SkipMakerRiskCheck: true,
		SkipTakerRiskCheck: true,
	}))
	require.Equal(t, 0, rk2.snapshots,
		"both flags on => no snapshots at all")
	require.Equal(t, 0, rk2.riskChecks,
		"both flags on => no IsValidRiskChangeFrom")

	// Sanity: with SkipTakerRiskCheck OFF (default), a forced taker
	// rejection (rejectOnCall=1, the for-loop iterates [Taker, Maker])
	// surfaces as `ErrTakerRiskRegression`, confirming the taker leg
	// IS reachable when the flag isn't set.
	ctx3, ak3, _, rk3, k3 := newSdkCtx(t)
	require.NoError(t, ak3.SetAccount(ctx3, accounttypes.Account{
		AccountIndex: 10, Collateral: math.NewInt(1_000_000),
	}))
	require.NoError(t, ak3.SetAccount(ctx3, accounttypes.Account{
		AccountIndex: 20, Collateral: math.NewInt(1_000_000),
	}))
	rk3.rejectOnCall = 1
	err := k3.ApplyPerpsMatching(ctx3, tradekeeper.PerpFill{
		MakerAccountIndex: 10,
		TakerAccountIndex: 20,
		MarketIndex:       1,
		Price:             100,
		BaseAmount:        5,
		IsTakerAsk:        true,
		NoFee:             true,
	})
	require.Error(t, err)
	require.ErrorIs(t, err, tradetypes.ErrTakerRiskRegression,
		"baseline (no Skip*RiskCheck) MUST classify the rejection as taker regression")
}

// TestApplySpotMatching_RejectsNegativeBalance ensures a buy-side spot trade
// against a zero-balance maker errors instead of writing a negative balance
// (audit High trade-8).
func TestApplySpotMatching_RejectsNegativeBalance(t *testing.T) {
	ctx, _, _, _, k := newSdkCtx(t)

	// Taker buys from maker, but maker has no base balance.
	err := k.ApplySpotMatching(ctx, tradekeeper.SpotFill{
		MakerAccountIndex: 100, TakerAccountIndex: 200,
		MarketIndex: 2, Price: 5, BaseAmount: 10,
		IsTakerAsk: false, NoFee: true,
	}, uint32(1), uint32(3))
	require.Error(t, err)
	require.Contains(t, err.Error(), "insufficient balance")
}
