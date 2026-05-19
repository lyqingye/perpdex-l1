// Package tests hosts the external-package tests for x/trade.
//
// setup_test.go centralises the per-test fixtures shared across the
// trade test suite: stub keepers (Account / Market / Funding / Risk)
// that mirror just enough of the production keeper surface to let the
// real `tradekeeper.Keeper` run end-to-end, plus the `newSdkCtx`
// helper that wires an in-memory KV store, encoding config, and the
// trade keeper itself.
//
// The stubs intentionally keep extra fixture-only setters (SetAccount,
// SetPosition, SetAccountAsset) that production interfaces in
// x/trade/types do NOT expose; they exist solely to seed state.
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
	risktypes "github.com/perpdex/perpdex-l1/x/risk/types"
	tradekeeper "github.com/perpdex/perpdex-l1/x/trade/keeper"
	tradetypes "github.com/perpdex/perpdex-l1/x/trade/types"
)

type stubAccount struct {
	accounts  map[uint64]*accounttypes.Account
	pos       map[[2]uint64]*accounttypes.AccountPosition
	assets    map[[2]uint64]*accounttypes.AccountAsset
	nextPosID uint64
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

// ApplyFill mirrors the production cohesive entry-point on
// x/account.Keeper (issue #91): classifies the transition from
// fill math (`AccountPosition.ApplyFill`) and persists with
// position_id allocation on open / flip. Production-level lifecycle
// enforcement (Open/Mutate/Close invariants, bounds check, event
// emission) lives on the real keeper; the stub keeps the maths
// faithful enough for the engine integration tests to assert
// position state + cross / isolated reconciliation end-to-end.
func (s *stubAccount) ApplyFill(
	ctx context.Context,
	accIdx uint64,
	marketIdx uint32,
	price uint32,
	baseDelta math.Int,
	fundingRatePrefixSum math.Int,
) (accounttypes.FillApplyResult, error) {
	pre, err := s.GetPosition(ctx, accIdx, marketIdx)
	if err != nil {
		return accounttypes.FillApplyResult{}, err
	}
	fill := pre.ApplyFill(baseDelta, price)

	maxPos := math.NewIntFromUint64(perptypes.MaxPositionSize)
	maxEntryQuote := math.NewIntFromUint64(perptypes.MaxEntryQuote)
	if fill.Position.BaseSize.Abs().GT(maxPos) || fill.Position.EntryQuote.Abs().GT(maxEntryQuote) {
		return accounttypes.FillApplyResult{}, accounttypes.ErrPositionOutOfBounds.Wrapf(
			"account %d market %d", accIdx, marketIdx)
	}

	res := accounttypes.FillApplyResult{
		Old:         pre,
		RealizedPnL: fill.RealizedPnL,
		SideFlipped: fill.SideFlipped,
		OIDelta:     fill.Position.BaseSize.Abs().Sub(pre.BaseSize.Abs()).Int64(),
	}

	switch {
	case pre.BaseSize.IsZero():
		// Open
		s.nextPosID++
		post := pre
		post.AccountIndex = accIdx
		post.MarketIndex = marketIdx
		post.BaseSize = fill.Position.BaseSize
		post.EntryQuote = fill.Position.EntryQuote
		post.PositionId = s.nextPosID
		if err := s.SetPosition(ctx, post); err != nil {
			return accounttypes.FillApplyResult{}, err
		}
		res.New = post
		return res, nil

	case fill.Position.BaseSize.IsZero():
		// Close — return pre-close snapshot, drop the row (or retain
		// leverage-only when non-default leverage).
		if pre.MarginMode != perptypes.CrossMargin || pre.InitialMarginFraction != 0 {
			leverage := accounttypes.AccountPosition{
				AccountIndex:             accIdx,
				MarketIndex:              marketIdx,
				BaseSize:                 math.ZeroInt(),
				EntryQuote:               math.ZeroInt(),
				LastFundingRatePrefixSum: math.ZeroInt(),
				AllocatedMargin:          math.ZeroInt(),
				MarginMode:               pre.MarginMode,
				InitialMarginFraction:    pre.InitialMarginFraction,
			}
			if err := s.SetPosition(ctx, leverage); err != nil {
				return accounttypes.FillApplyResult{}, err
			}
		} else {
			delete(s.pos, posKey(accIdx, marketIdx))
		}
		res.New = pre
		res.Closed = true
		return res, nil

	case fill.SideFlipped:
		// Flip = close old + open new, carry AllocatedMargin /
		// LastFundingRatePrefixSum onto the residual leg.
		closed := pre
		delete(s.pos, posKey(accIdx, marketIdx))
		s.nextPosID++
		post := closed
		post.BaseSize = fill.Position.BaseSize
		post.EntryQuote = fill.Position.EntryQuote
		post.PositionId = s.nextPosID
		if err := s.SetPosition(ctx, post); err != nil {
			return accounttypes.FillApplyResult{}, err
		}
		res.New = post
		return res, nil

	default:
		// Same-side update
		post := pre
		post.BaseSize = fill.Position.BaseSize
		post.EntryQuote = fill.Position.EntryQuote
		if err := s.SetPosition(ctx, post); err != nil {
			return accounttypes.FillApplyResult{}, err
		}
		res.New = post
		return res, nil
	}
}

// AdjustAllocatedMargin mirrors the cohesive isolated-margin RMW on
// x/account.Keeper. Used by the trade engine's isolated reconciliation
// (PnL/fee credit, improvement-fee debit, position_requirement
// rebalance) and by msg_server UpdateMargin.
func (s *stubAccount) AdjustAllocatedMargin(
	ctx context.Context,
	accIdx uint64,
	marketIdx uint32,
	delta math.Int,
) (accounttypes.AccountPosition, error) {
	pos, err := s.GetPosition(ctx, accIdx, marketIdx)
	if err != nil {
		return accounttypes.AccountPosition{}, err
	}
	if delta.IsNil() || delta.IsZero() {
		return pos, nil
	}
	pos.AllocatedMargin = pos.AllocatedMargin.Add(delta)
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
