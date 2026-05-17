// Package tests holds the integration-style tests for x/liquidation.
//
// This file is the shared fixture surface used by every test in the
// package:
//
//   - Bech32 prefix bootstrap (TestMain).
//   - In-memory stub keepers (account / market / risk / trade /
//     matching / funding) that satisfy the interfaces consumed by the
//     liquidation keeper, with just enough behaviour to drive the
//     LLP→ADL waterfall, the partial-liquidation IOC path and genesis
//     round-trips.
//   - Keeper construction helpers (newKeeper / newKeeperWithFunding)
//     that wire the in-memory store + stubs and seed default Params.
//
// Helpers live in `package tests` (no `_test` suffix), so every
// `*_test.go` file under x/liquidation/tests can call them without
// importing or re-declaring them. Nothing here is exported outside
// the package.
package tests

import (
	"context"
	"os"
	"sort"
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
	liqkeeper "github.com/perpdex/perpdex-l1/x/liquidation/keeper"
	liqtypes "github.com/perpdex/perpdex-l1/x/liquidation/types"
	markettypes "github.com/perpdex/perpdex-l1/x/market/types"
	risktypes "github.com/perpdex/perpdex-l1/x/risk/types"
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
		BaseSize: math.ZeroInt(), EntryQuote: math.ZeroInt(),
		LastFundingRatePrefixSum: math.ZeroInt(), AllocatedMargin: math.ZeroInt(),
	}, nil
}

// SetPosition is the stub-only fixture helper used by the suite.
// Production code never calls a generic position setter; the
// liquidation AccountKeeper interface only exposes UpdatePosition.
func (s *stubAccount) SetPosition(_ context.Context, p accounttypes.AccountPosition) error {
	s.pos[[2]uint64{p.AccountIndex, uint64(p.MarketIndex)}] = p
	return nil
}

// UpdatePosition mirrors the real keeper's RMW closure surface so
// any future liquidation write driven through the keeper interface
// keeps working in the in-memory stub. The current liquidation
// keeper does not invoke it, but the AccountKeeper interface
// requires it for parity with x/trade / x/funding.
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
	if a.Collateral.IsNil() {
		a.Collateral = math.ZeroInt()
	}
	a.Collateral = a.Collateral.Add(delta)
	a.AccountIndex = idx
	s.accounts[idx] = a
	return nil
}
func (s *stubAccount) IterateAccounts(_ context.Context, cb func(accounttypes.Account) bool) error {
	for _, a := range s.accounts {
		if cb(a) {
			return nil
		}
	}
	return nil
}

// IterateAccountPositions yields every persisted (acc, mkt) → position
// row in the in-memory map, mirroring the real keeper's prefix-iter
// semantics.
//
// The production iterator walks rows in ascending (account, market)
// order; we sort the stub's map output the same way so order-sensitive
// behaviour (attemptsLeft consumption ordering, ranking ties) stays
// reproducible across Go's randomised map iteration.
func (s *stubAccount) IterateAccountPositions(
	_ context.Context,
	accountIdx uint64,
	cb func(accounttypes.AccountPosition) bool,
) error {
	keys := make([][2]uint64, 0, len(s.pos))
	for k := range s.pos {
		if k[0] != accountIdx {
			continue
		}
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool { return keys[i][1] < keys[j][1] })
	for _, k := range keys {
		if cb(s.pos[k]) {
			return nil
		}
	}
	return nil
}
func (s *stubAccount) IsAuthorized(_ context.Context, signer string, idx uint64) (bool, error) {
	if a, ok := s.accounts[idx]; ok {
		return a.OwnerAddress == signer, nil
	}
	return false, nil
}

// stubMarket implements types.MarketKeeper for liquidation tests.
// When wired with a *stubRisk it delegates markPrice / MarketDetails reads
// to the risk stub's seeded tables so ADL ranking tests pick up the
// same per-market values they configured on `rk.marks`/`rk.mds`.
type stubMarket struct {
	rk *stubRisk
}

func (stubMarket) GetMarket(_ context.Context, idx uint32) (markettypes.Market, error) {
	return markettypes.Market{
		MarketIndex:    idx,
		MarketType:     perptypes.MarketTypePerps,
		LiquidationFee: 5_000, // 0.5% in fee tick units
	}, nil
}
func (s stubMarket) GetMarketDetails(_ context.Context, idx uint32) (markettypes.MarketDetails, error) {
	if s.rk != nil {
		return s.rk.mdFor(idx), nil
	}
	return markettypes.MarketDetails{MarketIndex: idx}, nil
}

// GetMarkPriceAndDetails delegates to the wired risk stub so ADL ranking
// tests share the per-market markPrice seeds; absent a wiring it returns a
// benign fresh fixture.
func (s stubMarket) GetMarkPriceAndDetails(_ context.Context, mkt uint32) (uint32, markettypes.MarketDetails, error) {
	if s.rk != nil {
		md := s.rk.mdFor(mkt)
		markPrice := s.rk.markFor(mkt)
		if md.LastMarkPriceRefreshTimestamp == 0 {
			md.LastMarkPriceRefreshTimestamp = 1
		}
		if md.MarkPrice == 0 {
			md.MarkPrice = markPrice
		}
		return markPrice, md, nil
	}
	return 1, markettypes.MarketDetails{MarketIndex: mkt, MarkPrice: 1, LastMarkPriceRefreshTimestamp: 1}, nil
}

// stubRisk implements types.RiskKeeper for liquidation tests.
// Per-account / per-market values are pre-loaded into the maps below;
// nothing is aggregated dynamically. The stub deliberately exposes
// ONLY the fields the production interface consumes (snapshot bundle,
// statuses, takeover preview) — internal computation steps are not
// modelled, so tests cannot accidentally rely on a partial production
// formula.
//
// `ak` is wired by `newKeeper` after construction so the stub can
// derive `snap.Position` from the same in-memory table the production
// account keeper would walk. `snapshotCalls` lets a test assert that
// the EndBlocker waterfall does not silently reuse stale aggregates
// across mutating calls.
type stubRisk struct {
	ak *stubAccount

	status     uint32                               // global default cross status
	statuses   map[uint64]uint32                    // per-account override (falls back to `status`)
	isoStat    map[[2]uint64]uint32                 // (acc, market) -> isolated status
	zero       map[[2]uint64]uint32                 // (acc, market) -> zero price override
	marks      map[uint32]uint32                    // market -> markPrice price (default 100)
	mds        map[uint32]markettypes.MarketDetails // market -> details
	cross      map[uint64]risktypes.RiskParameters  // account -> cross aggregate (overrides status projection)
	iso        map[[2]uint64]risktypes.RiskParameters
	postSim    map[uint64]risktypes.RiskParameters // takeover post-state per account
	postSimErr error                               // forced error from SimulateRiskAfterTakeover

	// snapshotCalls counts every GetLiquidationRiskSnapshot call;
	// tests use it to assert that the waterfall takes a fresh
	// snapshot per LLP / ADL invocation rather than caching across
	// mutations.
	snapshotCalls int
	// onSnapshot lets a test mutate stub state (e.g., flip a cross
	// account from FULL to HEALTHY) right before a snapshot is
	// returned, simulating a sibling fill having already mutated the
	// account.
	onSnapshot func(s *stubRisk, acc uint64, mkt uint32)
}

func newStubRisk() *stubRisk {
	return &stubRisk{
		statuses: map[uint64]uint32{},
		isoStat:  map[[2]uint64]uint32{},
		zero:     map[[2]uint64]uint32{},
		marks:    map[uint32]uint32{},
		mds:      map[uint32]markettypes.MarketDetails{},
		cross:    map[uint64]risktypes.RiskParameters{},
		iso:      map[[2]uint64]risktypes.RiskParameters{},
		postSim:  map[uint64]risktypes.RiskParameters{},
	}
}

func (s *stubRisk) GetHealthStatus(_ context.Context, acc uint64) (uint32, error) {
	return s.crossStatusFor(acc), nil
}
func (s *stubRisk) GetIsolatedHealthStatus(_ context.Context, acc uint64, mkt uint32) (uint32, error) {
	return s.isoStatusFor(acc, mkt), nil
}

// GetZeroPriceSnapshot mirrors the production lightweight snapshot:
// reads the position, short-circuits empty positions, and otherwise
// returns the test-seeded zero price (or `markPrice` as the default).
func (s *stubRisk) GetZeroPriceSnapshot(
	ctx context.Context, acc uint64, mkt uint32,
) (risktypes.ZeroPriceSnapshot, error) {
	pos, err := s.ak.GetPosition(ctx, acc, mkt)
	if err != nil {
		return risktypes.ZeroPriceSnapshot{}, err
	}
	if pos.BaseSize.IsZero() {
		return risktypes.ZeroPriceSnapshot{Position: pos}, nil
	}
	zp, ok := s.zero[[2]uint64{acc, uint64(mkt)}]
	if !ok {
		zp = s.markFor(mkt)
	}
	return risktypes.ZeroPriceSnapshot{Position: pos, ZeroPrice: zp}, nil
}

func (s *stubRisk) GetLiquidationRiskSnapshot(
	ctx context.Context, acc uint64, mkt uint32,
) (risktypes.LiquidationRiskSnapshot, error) {
	s.snapshotCalls++
	if s.onSnapshot != nil {
		s.onSnapshot(s, acc, mkt)
	}
	pos, err := s.ak.GetPosition(ctx, acc, mkt)
	if err != nil {
		return risktypes.LiquidationRiskSnapshot{}, err
	}
	markPrice := s.markFor(mkt)
	md := s.mdFor(mkt)
	crossRP, ok := s.cross[acc]
	if !ok {
		crossRP = riskParamsForStatus(s.crossStatusFor(acc))
	}
	risk := crossRP
	if pos.MarginMode == perptypes.IsolatedMargin {
		if v, found := s.iso[[2]uint64{acc, uint64(mkt)}]; found {
			risk = v
		} else {
			risk = riskParamsForStatus(s.isoStatusFor(acc, mkt))
		}
	}
	zp, ok := s.zero[[2]uint64{acc, uint64(mkt)}]
	if !ok {
		if pos.BaseSize.IsZero() {
			zp = 0
		} else {
			zp = markPrice
		}
	}
	return risktypes.LiquidationRiskSnapshot{
		Position:      pos,
		MarkPrice:     markPrice,
		MarketDetails: md,
		Risk:          risk,
		CrossRisk:     crossRP,
		ZeroPrice:     zp,
	}, nil
}

func (s *stubRisk) SimulateRiskAfterTakeover(
	_ context.Context, acc uint64, _ uint32, _ math.Int, _ uint32,
) (risktypes.RiskParameters, error) {
	if s.postSimErr != nil {
		return risktypes.RiskParameters{}, s.postSimErr
	}
	if v, ok := s.postSim[acc]; ok {
		return v, nil
	}
	return risktypes.RiskParameters{
		Collateral:                   math.NewInt(1_000_000),
		CollateralWithFunding:        math.NewInt(1_000_000),
		TotalAccountValue:            math.NewInt(1_000_000),
		InitialMarginRequirement:     math.ZeroInt(),
		MaintenanceMarginRequirement: math.ZeroInt(),
		CloseOutMarginRequirement:    math.ZeroInt(),
	}, nil
}

func (s *stubRisk) crossStatusFor(acc uint64) uint32 {
	if v, ok := s.statuses[acc]; ok {
		return v
	}
	return s.status
}

func (s *stubRisk) isoStatusFor(acc uint64, mkt uint32) uint32 {
	if v, ok := s.isoStat[[2]uint64{acc, uint64(mkt)}]; ok {
		return v
	}
	return perptypes.HealthHealthy
}

func (s *stubRisk) markFor(mkt uint32) uint32 {
	if v, ok := s.marks[mkt]; ok {
		return v
	}
	return 100
}

func (s *stubRisk) mdFor(mkt uint32) markettypes.MarketDetails {
	if v, ok := s.mds[mkt]; ok {
		return v
	}
	return markettypes.MarketDetails{MarketIndex: mkt}
}

// riskParamsForStatus projects a status enum into a RiskParameters
// that the production classifier round-trips back to the same status.
// Used by the stub when tests seed `status` / `statuses` / `isoStat`
// tables instead of full RP fixtures.
func riskParamsForStatus(status uint32) risktypes.RiskParameters {
	z := math.ZeroInt()
	rp := risktypes.RiskParameters{
		Collateral:                   z,
		CollateralWithFunding:        z,
		TotalAccountValue:            z,
		InitialMarginRequirement:     z,
		MaintenanceMarginRequirement: z,
		CloseOutMarginRequirement:    z,
	}
	switch status {
	case perptypes.HealthBankruptcy:
		rp.TotalAccountValue = math.NewInt(-1)
	case perptypes.HealthFullLiquidation:
		rp.TotalAccountValue = math.ZeroInt()
		rp.InitialMarginRequirement = math.NewInt(3)
		rp.MaintenanceMarginRequirement = math.NewInt(2)
		rp.CloseOutMarginRequirement = math.NewInt(1)
	case perptypes.HealthPartialLiquidation:
		rp.TotalAccountValue = math.NewInt(1)
		rp.InitialMarginRequirement = math.NewInt(3)
		rp.MaintenanceMarginRequirement = math.NewInt(2)
		rp.CloseOutMarginRequirement = math.ZeroInt()
	case perptypes.HealthPreLiquidation:
		rp.TotalAccountValue = math.NewInt(1)
		rp.InitialMarginRequirement = math.NewInt(2)
	default:
	}
	return rp
}

type stubTrade struct {
	calls []tradekeeper.PerpFill
	// err lets tests force ApplyPerpsMatching to fail (used to
	// model post-trade risk regression on bankrupt or recoverable
	// rejections).
	err error
	// errFn lets tests fail individual fills based on the PerpFill
	// payload — used to model the trade engine rejecting a single
	// counterparty (e.g. ErrTakerRiskRegression) while letting the
	// next ranked candidate proceed. When set, errFn takes
	// precedence over the static `err` field. The fill is still
	// recorded in `calls` regardless of the returned error.
	errFn func(f tradekeeper.PerpFill) error
	// onCall lets a test mutate sibling stub state right after a
	// fill is recorded — used to model the cross account's TAV /
	// status changing as a result of an LLP / ADL fill so the next
	// position iteration must observe the mutation.
	onCall func(f tradekeeper.PerpFill)
}

func (s *stubTrade) ApplyPerpsMatching(_ context.Context, f tradekeeper.PerpFill) error {
	s.calls = append(s.calls, f)
	if s.onCall != nil {
		s.onCall(f)
	}
	if s.errFn != nil {
		return s.errFn(f)
	}
	return s.err
}

// stubFunding satisfies liquidation/types.FundingKeeper. The real
// keeper writes to position state via UpdatePosition; the stub
// exposes a per-(acc,mkt) call counter so tests can assert that the
// pre-trade collateral assert in Deleverage settles funding before
// reading available collateral.
type stubFunding struct {
	calls map[[2]uint64]int
	err   error
}

func newStubFunding() *stubFunding { return &stubFunding{calls: map[[2]uint64]int{}} }

func (s *stubFunding) SettlePositionFunding(_ context.Context, acc uint64, mkt uint32) error {
	s.calls[[2]uint64{acc, uint64(mkt)}]++
	return s.err
}

// liqOrderCall captures every MatchLiquidationOrder argument for the
// new orderbook-IOC partial-liquidation path.
type liqOrderCall struct {
	Victim                  uint64
	MarketIdx               uint32
	ZeroPrice               uint32
	BaseAmount              uint64
	LiquidationFeeBps       uint32
	LiquidationFeeRecipient uint64
}

type stubMatching struct {
	cancelled map[uint64]uint32
	// liqCalls records every MatchLiquidationOrder invocation. Tests
	// assert against the synthetic taker's parameters since the
	// real matching engine + trade engine path is exercised in
	// x/matching's internal tests.
	liqCalls []liqOrderCall
	// liqFilled is the filled-base value the stub reports back.
	// Defaults to baseAmount (full fill); tests can override via
	// `liqFilledOverride` to model partial fills.
	liqFilledOverride *uint64
	// liqErr lets a test inject an error from MatchLiquidationOrder
	// without having to patch the stub.
	liqErr error
}

func newStubMatching() *stubMatching { return &stubMatching{cancelled: map[uint64]uint32{}} }

func (s *stubMatching) CancelAllOpenOrdersForAccount(_ context.Context, acc uint64) (uint32, error) {
	s.cancelled[acc]++
	return 0, nil
}

func (s *stubMatching) MatchLiquidationOrder(
	_ context.Context,
	victim uint64,
	marketIdx uint32,
	zeroPrice uint32,
	baseAmount uint64,
	liquidationFeeBps uint32,
	liquidationFeeRecipient uint64,
) (uint64, error) {
	s.liqCalls = append(s.liqCalls, liqOrderCall{
		Victim:                  victim,
		MarketIdx:               marketIdx,
		ZeroPrice:               zeroPrice,
		BaseAmount:              baseAmount,
		LiquidationFeeBps:       liquidationFeeBps,
		LiquidationFeeRecipient: liquidationFeeRecipient,
	})
	if s.liqErr != nil {
		return 0, s.liqErr
	}
	if s.liqFilledOverride != nil {
		return *s.liqFilledOverride, nil
	}
	return baseAmount, nil
}

func newKeeper(
	t *testing.T,
	ak *stubAccount, rk *stubRisk, tk *stubTrade, matchk *stubMatching,
) (liqkeeper.Keeper, sdk.Context) {
	t.Helper()
	return newKeeperWithFunding(t, ak, rk, tk, matchk, newStubFunding())
}

func newKeeperWithFunding(
	t *testing.T,
	ak *stubAccount, rk *stubRisk, tk *stubTrade, matchk *stubMatching, fk *stubFunding,
) (liqkeeper.Keeper, sdk.Context) {
	t.Helper()
	// Wire the account stub into the risk stub so
	// GetLiquidationRiskSnapshot can derive Position from the same
	// in-memory table the production keeper would walk.
	rk.ak = ak
	keys := storetypes.NewKVStoreKeys(liqtypes.StoreKey)
	cdc := moduletestutil.MakeTestEncodingConfig().Codec
	cms := integration.CreateMultiStore(keys, log.NewTestLogger(t))
	ctx := sdk.NewContext(cms, cmtprototypes.Header{}, true, log.NewTestLogger(t))
	k := liqkeeper.NewKeeper(
		cdc,
		runtime.NewKVStoreService(keys[liqtypes.StoreKey]),
		"px1qv9pzxqlyckngw6zf9g9whn9d3eh4qvgsxc8cx",
		ak,
		stubMarket{rk: rk},
		rk,
		tk,
		matchk,
		fk,
	)
	// Seed Params so EndBlocker can read MaxAdlAttemptsPerBlock /
	// MaxAdlCandidatesPerVictim. Without this each EndBlocker invocation
	// fails with `collections: not found`.
	if err := k.Params.Set(ctx, liqtypes.DefaultParams()); err != nil {
		t.Fatalf("seed params: %v", err)
	}
	return k, ctx
}
