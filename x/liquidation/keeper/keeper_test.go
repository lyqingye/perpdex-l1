package keeper_test

import (
	"context"
	"os"
	"sort"
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
	risktypes "github.com/perpdex/perpdex-l1/x/risk/types"
	tradekeeper "github.com/perpdex/perpdex-l1/x/trade/keeper"
)

func TestMain(m *testing.M) {
	cfg := sdk.GetConfig()
	cfg.SetBech32PrefixForAccount("px", "pxpub")
	os.Exit(m.Run())
}

// ----------- stubs -----------

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
// semantics. processAccount / rankVictimPositionsByUPnL use this in
// place of the legacy MaxPerpsMarketIndex full-scan loops.
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

type stubMarket struct{}

func (stubMarket) GetMarket(_ context.Context, idx uint32) (markettypes.Market, error) {
	return markettypes.Market{
		MarketIndex:    idx,
		MarketType:     perptypes.MarketTypePerps,
		LiquidationFee: 5_000, // 0.5% in fee tick units
	}, nil
}
func (stubMarket) GetMarketDetails(_ context.Context, idx uint32) (markettypes.MarketDetails, error) {
	return markettypes.MarketDetails{MarketIndex: idx}, nil
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
// account keeper would walk. `riskInfoCalls` lets a test assert that
// the EndBlocker waterfall does not silently reuse stale aggregates
// across mutating calls (see TestEndBlocker_StaleCrossAggregateRefresh).
type stubRisk struct {
	ak *stubAccount

	status   uint32                               // global default cross status
	statuses map[uint64]uint32                    // per-account override (falls back to `status`)
	isoStat  map[[2]uint64]uint32                 // (acc, market) -> isolated status
	zero     map[[2]uint64]uint32                 // (acc, market) -> zero price override
	marks    map[uint32]uint32                    // market -> mark price (default 100)
	mds      map[uint32]markettypes.MarketDetails // market -> details
	cross    map[uint64]risktypes.RiskParameters  // account -> cross aggregate (overrides status projection)
	iso      map[[2]uint64]risktypes.RiskParameters
	postSim  map[uint64]risktypes.RiskParameters // takeover post-state per account
	postSimErr error                              // forced error from SimulateRiskAfterTakeover

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
func (s *stubRisk) GetPositionZeroPrice(_ context.Context, acc uint64, mkt uint32) (uint32, error) {
	if v, ok := s.zero[[2]uint64{acc, uint64(mkt)}]; ok {
		return v, nil
	}
	return s.markFor(mkt), nil
}
func (s *stubRisk) GetMarkAndMarketDetails(_ context.Context, mkt uint32) (uint32, markettypes.MarketDetails, error) {
	return s.markFor(mkt), s.mdFor(mkt), nil
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
	mark := s.markFor(mkt)
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
		if pos.Position.IsZero() {
			zp = 0
		} else {
			zp = mark
		}
	}
	return risktypes.LiquidationRiskSnapshot{
		Position:      pos,
		MarkPrice:     mark,
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
		stubMarket{},
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

// ----------- legacy tests retained verbatim (semantics unchanged) -----------

// TestLiquidate_BaseAmountCappedByPosition ensures that passing a
// base_amount greater than the victim's position is rejected rather than
// silently flipping the position (audit Blocker liquidation-2).
func TestLiquidate_BaseAmountCappedByPosition(t *testing.T) {
	ak := newStubAccount()
	ak.accounts[100] = accounttypes.Account{AccountIndex: 100, Collateral: math.ZeroInt()}
	ak.pos[[2]uint64{100, 0}] = accounttypes.AccountPosition{
		AccountIndex: 100, MarketIndex: 0,
		Position: math.NewInt(5), EntryQuote: math.NewInt(-50),
		LastFundingRatePrefixSum: math.ZeroInt(), AllocatedMargin: math.ZeroInt(),
	}
	rk := newStubRisk()
	rk.status = perptypes.HealthPartialLiquidation
	tk := &stubTrade{}
	matchk := newStubMatching()
	k, ctx := newKeeper(t, ak, rk, tk, matchk)

	err := k.Liquidate(ctx, 100, 0, 999 /* > victim size */)
	require.Error(t, err)
	require.ErrorIs(t, err, liqtypes.ErrInvalidParams)
	require.Empty(t, matchk.liqCalls,
		"oversized base must reject before MatchLiquidationOrder is invoked")
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
	rk := newStubRisk()
	rk.status = perptypes.HealthPartialLiquidation
	tk := &stubTrade{}
	matchk := newStubMatching()
	k, ctx := newKeeper(t, ak, rk, tk, matchk)

	err := k.Liquidate(ctx, 100, 0, 0)
	require.ErrorIs(t, err, liqtypes.ErrInvalidParams)
}

// TestLiquidate_RejectsFullLiquidationStatus verifies that MsgLiquidate
// only services PARTIAL_LIQUIDATION; FULL/BANKRUPTCY accounts must
// fall through to the EndBlocker LLP→ADL waterfall (Lighter
// `InternalDeleverageTx` path).
func TestLiquidate_RejectsFullLiquidationStatus(t *testing.T) {
	ak := newStubAccount()
	ak.accounts[100] = accounttypes.Account{AccountIndex: 100, Collateral: math.ZeroInt()}
	ak.pos[[2]uint64{100, 0}] = accounttypes.AccountPosition{
		AccountIndex: 100, MarketIndex: 0, Position: math.NewInt(3),
		EntryQuote: math.NewInt(-30), LastFundingRatePrefixSum: math.ZeroInt(),
		AllocatedMargin: math.ZeroInt(),
	}
	rk := newStubRisk()
	rk.status = perptypes.HealthFullLiquidation
	tk := &stubTrade{}
	matchk := newStubMatching()
	k, ctx := newKeeper(t, ak, rk, tk, matchk)

	err := k.Liquidate(ctx, 100, 0, 1)
	require.ErrorIs(t, err, liqtypes.ErrNotLiquidatable)
	require.Empty(t, matchk.liqCalls,
		"FULL victim must not enter the matching path")

	rk.status = perptypes.HealthBankruptcy
	err = k.Liquidate(ctx, 100, 0, 1)
	require.ErrorIs(t, err, liqtypes.ErrNotLiquidatable,
		"BANKRUPTCY must also reject the partial-liquidation route")
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
	rk := newStubRisk()
	rk.status = perptypes.HealthFullLiquidation
	tk := &stubTrade{}
	matchk := newStubMatching()
	k, ctx := newKeeper(t, ak, rk, tk, matchk)
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

// ----------- new Lighter-parity tests -----------

// TestLiquidate_CancelsVictimOpenOrders verifies the spec rule "first
// cancel all open orders of the user" before submitting the
// liquidation IOC to the matching keeper. Lighter parity:
// `InternalCancelAllOrdersTx → InternalLiquidatePositionTx`.
func TestLiquidate_CancelsVictimOpenOrders(t *testing.T) {
	ak := newStubAccount()
	ak.accounts[100] = accounttypes.Account{AccountIndex: 100, Collateral: math.ZeroInt()}
	ak.pos[[2]uint64{100, 0}] = accounttypes.AccountPosition{
		AccountIndex: 100, MarketIndex: 0,
		Position: math.NewInt(10), EntryQuote: math.NewInt(-100),
		LastFundingRatePrefixSum: math.ZeroInt(), AllocatedMargin: math.ZeroInt(),
	}
	rk := newStubRisk()
	rk.status = perptypes.HealthPartialLiquidation
	tk := &stubTrade{}
	matchk := newStubMatching()
	k, ctx := newKeeper(t, ak, rk, tk, matchk)

	err := k.Liquidate(ctx, 100, 0, 5)
	require.NoError(t, err)
	require.Equal(t, uint32(1), matchk.cancelled[100],
		"victim must have orders cancelled before close-out")
	require.Len(t, matchk.liqCalls, 1,
		"matching keeper must receive exactly one liquidation IOC")
}

// TestLiquidate_DelegatesToMatchingKeeperWithLLPRecipient verifies
// that Keeper.Liquidate forwards the close-out to the matching keeper
// with the right victim / market / zero price / fee bps and routes the
// improvement fee to the LLP / Insurance Fund operator account.
func TestLiquidate_DelegatesToMatchingKeeperWithLLPRecipient(t *testing.T) {
	ak := newStubAccount()
	ak.accounts[100] = accounttypes.Account{AccountIndex: 100, Collateral: math.ZeroInt()}
	ak.pos[[2]uint64{100, 0}] = accounttypes.AccountPosition{
		AccountIndex: 100, MarketIndex: 0,
		Position: math.NewInt(10), EntryQuote: math.NewInt(-1000),
		LastFundingRatePrefixSum: math.ZeroInt(), AllocatedMargin: math.ZeroInt(),
	}
	rk := newStubRisk()
	rk.status = perptypes.HealthPartialLiquidation
	rk.zero[[2]uint64{100, 0}] = 95 // mark-based zero price
	tk := &stubTrade{}
	matchk := newStubMatching()
	k, ctx := newKeeper(t, ak, rk, tk, matchk)

	err := k.Liquidate(ctx, 100, 0, 4)
	require.NoError(t, err)
	require.Len(t, matchk.liqCalls, 1)
	got := matchk.liqCalls[0]
	require.Equal(t, uint64(100), got.Victim)
	require.Equal(t, uint32(0), got.MarketIdx)
	require.Equal(t, uint32(95), got.ZeroPrice)
	require.Equal(t, uint64(4), got.BaseAmount)
	require.Greater(t, got.LiquidationFeeBps, uint32(0),
		"market.LiquidationFee must populate the fee bps on the IOC call")
	require.Equal(t, perptypes.InsuranceFundOperatorAccountIdx, got.LiquidationFeeRecipient,
		"liquidation fee must route to LLP / Insurance Fund operator")
	// Liquidate path no longer invokes ApplyPerpsMatching directly
	// — that is the matching keeper's job.
	require.Empty(t, tk.calls,
		"liquidation must not bypass the orderbook IOC route")
}

// TestEndBlocker_FullLiquidationPrefersLLPThenADL exercises the
// FULL_LIQUIDATION branch: the LLP (IF) is offered the worst-uPnL
// position first; if it accepts, no ADL fill is generated.
func TestEndBlocker_FullLiquidationPrefersLLPThenADL(t *testing.T) {
	ak := newStubAccount()
	// Insurance Fund pool (idx 1, ACTIVE).
	ak.accounts[perptypes.InsuranceFundOperatorAccountIdx] = accounttypes.Account{
		AccountIndex: perptypes.InsuranceFundOperatorAccountIdx,
		AccountType:  perptypes.InsuranceFundAccountType,
		Collateral:   math.NewInt(10_000_000),
		PublicPoolInfo: &accounttypes.PublicPoolInfo{
			Status:         perptypes.PublicPoolStatusActive,
			TotalShares:    math.NewInt(1),
			OperatorShares: math.NewInt(1),
		},
	}
	// Victim with one FULL_LIQUIDATION cross position. EntryQuote
	// follows the production canonical sign (long → positive
	// notional in) so the pre-trade collateral assert can pass —
	// closing this position at zeroPrice=10 yields a realised PnL
	// of +4_500 in the engine's "Collateral += RealizedPnL" frame.
	ak.accounts[100] = accounttypes.Account{AccountIndex: 100, Collateral: math.NewInt(10)}
	ak.pos[[2]uint64{100, 0}] = accounttypes.AccountPosition{
		AccountIndex: 100, MarketIndex: 0,
		Position: math.NewInt(50), EntryQuote: math.NewInt(5_000),
		LastFundingRatePrefixSum: math.ZeroInt(), AllocatedMargin: math.ZeroInt(),
	}
	rk := newStubRisk()
	rk.status = perptypes.HealthFullLiquidation
	// uPnL ordering is derived from `pos.UnrealizedPnL(mark)`; victim
	// 100 holds (Position=50, EntryQuote=5000), so at the stub default
	// mark=100 the uPnL is -4500 (loss). Single position → trivially
	// the worst.
	tk := &stubTrade{}
	matchk := newStubMatching()
	k, ctx := newKeeper(t, ak, rk, tk, matchk)

	require.NoError(t, k.EndBlocker(ctx))

	// Exactly one fill, target = IF as taker.
	require.Len(t, tk.calls, 1, "LLP absorb should produce one fill")
	require.Equal(t, perptypes.InsuranceFundOperatorAccountIdx, tk.calls[0].TakerAccountIndex,
		"counterparty must be the insurance fund operator")
	require.True(t, tk.calls[0].NoFee, "LLP takeover is a fee-less close")
	require.True(t, tk.calls[0].SkipTakerRiskCheck,
		"LLP / IF deleverager bypasses post-trade taker risk check (Lighter parity)")
	require.False(t, tk.calls[0].SkipMakerRiskCheck,
		"bankrupt (maker) post-trade risk check must remain enabled")
}

// TestLLPAbsorb_StopsWhenLLPWouldBreachIMR verifies that the LLP IMR
// safety gate blocks takeover when SimulateRiskAfterTakeover reports
// post.TAV < post.IMR; the position falls through to ADL instead.
func TestLLPAbsorb_StopsWhenLLPWouldBreachIMR(t *testing.T) {
	ak := newStubAccount()
	ak.accounts[perptypes.InsuranceFundOperatorAccountIdx] = accounttypes.Account{
		AccountIndex: perptypes.InsuranceFundOperatorAccountIdx,
		AccountType:  perptypes.InsuranceFundAccountType,
		Collateral:   math.NewInt(100), // tiny; can't absorb
		PublicPoolInfo: &accounttypes.PublicPoolInfo{
			Status:         perptypes.PublicPoolStatusActive,
			TotalShares:    math.NewInt(1),
			OperatorShares: math.NewInt(1),
		},
	}
	// One ADL counterparty (account 999) on the opposite side, in
	// profit, so autoADL has someone to fill against.
	ak.accounts[999] = accounttypes.Account{
		AccountIndex: 999,
		AccountType:  perptypes.MasterAccountType,
		Collateral:   math.NewInt(1_000_000),
	}
	ak.pos[[2]uint64{999, 0}] = accounttypes.AccountPosition{
		AccountIndex: 999, MarketIndex: 0,
		Position: math.NewInt(-10), EntryQuote: math.NewInt(-2_000),
		LastFundingRatePrefixSum: math.ZeroInt(), AllocatedMargin: math.ZeroInt(),
	}
	// Bankrupt has a small but non-zero cushion so the autoADL pre-
	// trade collateral assert (Lighter `is_bankrupt_has_enough_cross_
	// collateral`) passes at the candidate's settle price (mid of
	// 100/110 = 105). The realised PnL on a 10-unit close at 105
	// against EQ=+5000 is small (~50), so 100 of cushion is plenty.
	ak.accounts[100] = accounttypes.Account{AccountIndex: 100, Collateral: math.NewInt(100)}
	ak.pos[[2]uint64{100, 0}] = accounttypes.AccountPosition{
		AccountIndex: 100, MarketIndex: 0,
		Position: math.NewInt(50), EntryQuote: math.NewInt(5_000),
		LastFundingRatePrefixSum: math.ZeroInt(), AllocatedMargin: math.ZeroInt(),
	}
	rk := newStubRisk()
	rk.status = perptypes.HealthFullLiquidation
	// At default mark=100: victim 100 (50, 5000) → uPnL=-4500 (worst).
	// Cand 999 (-10, -2000) → uPnL=1000 (>0, qualifies as ADL cand).
	rk.zero[[2]uint64{100, 0}] = 100
	rk.zero[[2]uint64{999, 0}] = 110
	// LLP would breach IMR: simulate post-state with TAV < IMR.
	rk.postSim[perptypes.InsuranceFundOperatorAccountIdx] = risktypes.RiskParameters{
		Collateral:                   math.NewInt(100),
		TotalAccountValue:            math.NewInt(50),
		InitialMarginRequirement:     math.NewInt(500), // breaches
		MaintenanceMarginRequirement: math.NewInt(250),
		CloseOutMarginRequirement:    math.NewInt(125),
	}
	tk := &stubTrade{}
	matchk := newStubMatching()
	k, ctx := newKeeper(t, ak, rk, tk, matchk)

	require.NoError(t, k.EndBlocker(ctx))

	// No LLP fill should be issued; instead an ADL fill (taker = 999).
	require.NotEmpty(t, tk.calls, "ADL must run when LLP refuses")
	for _, f := range tk.calls {
		require.NotEqual(t, perptypes.InsuranceFundOperatorAccountIdx, f.TakerAccountIndex,
			"LLP must not be taker when IMR check fails")
	}
}

// TestEndBlocker_BankruptcyFallsThroughToADLWhenLLPBreachesIMR
// verifies the spec rule: a deeply bankrupt account whose absorption
// would breach the LLP's IMR is closed via ADL instead, leaving the
// LLP untouched.
func TestEndBlocker_BankruptcyFallsThroughToADLWhenLLPBreachesIMR(t *testing.T) {
	ak := newStubAccount()
	ak.accounts[perptypes.InsuranceFundOperatorAccountIdx] = accounttypes.Account{
		AccountIndex: perptypes.InsuranceFundOperatorAccountIdx,
		AccountType:  perptypes.InsuranceFundAccountType,
		Collateral:   math.NewInt(100), // tiny; absorbing this position breaches IMR
		PublicPoolInfo: &accounttypes.PublicPoolInfo{
			Status: perptypes.PublicPoolStatusActive,
		},
	}
	ak.accounts[999] = accounttypes.Account{
		AccountIndex: 999, AccountType: perptypes.MasterAccountType,
		Collateral: math.NewInt(1_000_000),
	}
	ak.pos[[2]uint64{999, 0}] = accounttypes.AccountPosition{
		AccountIndex: 999, MarketIndex: 0,
		Position: math.NewInt(-10), EntryQuote: math.NewInt(-2_000),
		LastFundingRatePrefixSum: math.ZeroInt(), AllocatedMargin: math.ZeroInt(),
	}
	// Victim is BANKRUPT but the trade-mechanical realised PnL still
	// has to fit available collateral; the test gives the bankrupt a
	// modest cushion (300) so the pre-trade collateral assert in
	// autoADL can pass at the candidate's settle price.
	ak.accounts[100] = accounttypes.Account{AccountIndex: 100, Collateral: math.NewInt(300)}
	ak.pos[[2]uint64{100, 0}] = accounttypes.AccountPosition{
		AccountIndex: 100, MarketIndex: 0,
		Position: math.NewInt(50), EntryQuote: math.NewInt(10_000),
		LastFundingRatePrefixSum: math.ZeroInt(), AllocatedMargin: math.ZeroInt(),
	}
	rk := newStubRisk()
	rk.status = perptypes.HealthBankruptcy
	rk.zero[[2]uint64{100, 0}] = 100
	rk.zero[[2]uint64{999, 0}] = 110
	// At default mark=100, cand 999 (-10, -2000) → uPnL=1000 (>0).
	rk.postSim[perptypes.InsuranceFundOperatorAccountIdx] = risktypes.RiskParameters{
		Collateral:                   math.NewInt(100),
		TotalAccountValue:            math.NewInt(50),
		InitialMarginRequirement:     math.NewInt(500), // breaches IMR
		MaintenanceMarginRequirement: math.NewInt(250),
		CloseOutMarginRequirement:    math.NewInt(125),
	}
	tk := &stubTrade{}
	matchk := newStubMatching()
	k, ctx := newKeeper(t, ak, rk, tk, matchk)

	require.NoError(t, k.EndBlocker(ctx))

	// No LLP takeover.
	for _, f := range tk.calls {
		require.NotEqual(t, perptypes.InsuranceFundOperatorAccountIdx, f.TakerAccountIndex,
			"LLP must not be taker when IMR check fails")
	}
	require.NotEmpty(t, tk.calls)
	require.Equal(t, uint64(999), tk.calls[0].TakerAccountIndex)
}

// TestAutoADL_RequiresZeroPriceAlignment verifies that ADL skips
// counterparties whose zero prices do NOT overlap with the victim's,
// which prevents the close-out from worsening the counterparty.
//
// The bankrupt is given a small but non-trivial collateral cushion so
// the pre-trade collateral assert (Lighter
// `is_bankrupt_has_enough_cross_collateral` parity) passes at the
// candidate settle price; the test's interest is purely the ZP
// alignment filter, not the collateral guard.
func TestAutoADL_RequiresZeroPriceAlignment(t *testing.T) {
	ak := newStubAccount()
	ak.accounts[100] = accounttypes.Account{AccountIndex: 100, Collateral: math.NewInt(200)}
	ak.pos[[2]uint64{100, 0}] = accounttypes.AccountPosition{
		AccountIndex: 100, MarketIndex: 0,
		Position: math.NewInt(50), EntryQuote: math.NewInt(5_000),
		LastFundingRatePrefixSum: math.ZeroInt(), AllocatedMargin: math.ZeroInt(),
	}
	// Two opposite-side candidates, but only one has an aligned ZP.
	ak.accounts[201] = accounttypes.Account{
		AccountIndex: 201, AccountType: perptypes.MasterAccountType,
		Collateral: math.NewInt(1_000_000),
	}
	ak.pos[[2]uint64{201, 0}] = accounttypes.AccountPosition{
		AccountIndex: 201, MarketIndex: 0,
		Position: math.NewInt(-10), EntryQuote: math.NewInt(-1_500),
		LastFundingRatePrefixSum: math.ZeroInt(), AllocatedMargin: math.ZeroInt(),
	}
	ak.accounts[202] = accounttypes.Account{
		AccountIndex: 202, AccountType: perptypes.MasterAccountType,
		Collateral: math.NewInt(1_000_000),
	}
	ak.pos[[2]uint64{202, 0}] = accounttypes.AccountPosition{
		AccountIndex: 202, MarketIndex: 0,
		Position: math.NewInt(-20), EntryQuote: math.NewInt(-2_500),
		LastFundingRatePrefixSum: math.ZeroInt(), AllocatedMargin: math.ZeroInt(),
	}
	rk := newStubRisk()
	rk.status = perptypes.HealthBankruptcy
	rk.zero[[2]uint64{100, 0}] = 100 // victim long ZP = 100 (need ZP_cand >= 100)
	rk.zero[[2]uint64{201, 0}] = 90  // misaligned: ZP < victim — skip
	rk.zero[[2]uint64{202, 0}] = 105 // aligned
	// At default mark=100, both shorts (Position=-10, EQ=-1500) and
	// (Position=-20, EQ=-2500) have positive uPnL (500 each), so they
	// both qualify as ADL candidates.
	tk := &stubTrade{}
	matchk := newStubMatching()
	k, ctx := newKeeper(t, ak, rk, tk, matchk)

	require.NoError(t, k.EndBlocker(ctx))

	// ADL should have happened against 202 only, at midpoint of 100 and
	// 105 = 102.
	require.NotEmpty(t, tk.calls)
	for _, f := range tk.calls {
		require.NotEqual(t, uint64(201), f.TakerAccountIndex,
			"misaligned ZP candidate must be skipped")
	}
	require.Equal(t, uint64(202), tk.calls[0].TakerAccountIndex)
	require.Equal(t, uint32(102), tk.calls[0].Price)
}

// TestADLQueueBuilder_LeverageAndUPnLRanking verifies the new ranking
// semantics: candidates are ordered by leverage * uPnL_ratio desc.
func TestADLQueueBuilder_LeverageAndUPnLRanking(t *testing.T) {
	ak := newStubAccount()
	// Two opposite-side longs with identical uPnL but different
	// leverages — higher leverage must come first.
	ak.accounts[201] = accounttypes.Account{
		AccountIndex: 201, AccountType: perptypes.MasterAccountType,
		Collateral: math.NewInt(10_000_000), // low leverage
	}
	ak.accounts[202] = accounttypes.Account{
		AccountIndex: 202, AccountType: perptypes.MasterAccountType,
		Collateral: math.NewInt(100_000), // high leverage
	}
	ak.pos[[2]uint64{201, 0}] = accounttypes.AccountPosition{
		AccountIndex: 201, MarketIndex: 0,
		Position: math.NewInt(10), EntryQuote: math.NewInt(1_000),
		LastFundingRatePrefixSum: math.ZeroInt(), AllocatedMargin: math.ZeroInt(),
	}
	ak.pos[[2]uint64{202, 0}] = accounttypes.AccountPosition{
		AccountIndex: 202, MarketIndex: 0,
		Position: math.NewInt(10), EntryQuote: math.NewInt(1_000),
		LastFundingRatePrefixSum: math.ZeroInt(), AllocatedMargin: math.ZeroInt(),
	}
	rk := newStubRisk()
	// Set mark=110 so both candidates' positions (Pos=10, EQ=1000)
	// realise uPnL=100 (=10*110-1000), giving an equal uPnLRatio so
	// ranking is decided purely by leverage (higher first).
	rk.marks[0] = 110
	rk.cross[201] = risktypes.RiskParameters{
		Collateral:                   math.NewInt(10_000_000),
		TotalAccountValue:            math.NewInt(10_000_000),
		InitialMarginRequirement:     math.NewInt(1_000),
		MaintenanceMarginRequirement: math.NewInt(500),
		CloseOutMarginRequirement:    math.NewInt(250),
	}
	rk.cross[202] = risktypes.RiskParameters{
		Collateral:                   math.NewInt(100_000),
		TotalAccountValue:            math.NewInt(100_000),
		InitialMarginRequirement:     math.NewInt(1_000),
		MaintenanceMarginRequirement: math.NewInt(500),
		CloseOutMarginRequirement:    math.NewInt(250),
	}
	tk := &stubTrade{}
	matchk := newStubMatching()
	k, ctx := newKeeper(t, ak, rk, tk, matchk)

	cands, err := k.BuildADLQueue(ctx, 0, true /* oppositeIsLong: victim is short */, 4)
	require.NoError(t, err)
	require.Len(t, cands, 2)
	require.Equal(t, uint64(202), cands[0].AccountIndex,
		"higher-leverage candidate must rank first")
	require.Equal(t, uint64(201), cands[1].AccountIndex)
}

// TestEndBlocker_PreLiquidationClearsFlags ensures the EndBlocker
// drops stale liquidation flags as soon as the account's health
// recovers to PRE / HEALTHY (no flag should persist into a healthy
// account's records).
func TestEndBlocker_PreLiquidationClearsFlags(t *testing.T) {
	ak := newStubAccount()
	ak.accounts[100] = accounttypes.Account{AccountIndex: 100, Collateral: math.NewInt(10_000)}
	ak.pos[[2]uint64{100, 0}] = accounttypes.AccountPosition{
		AccountIndex: 100, MarketIndex: 0,
		Position: math.NewInt(5), EntryQuote: math.NewInt(-50),
		LastFundingRatePrefixSum: math.ZeroInt(), AllocatedMargin: math.ZeroInt(),
	}
	rk := newStubRisk()
	rk.status = perptypes.HealthPreLiquidation
	tk := &stubTrade{}
	matchk := newStubMatching()
	k, ctx := newKeeper(t, ak, rk, tk, matchk)

	require.NoError(t, k.EndBlocker(ctx))
	// PRE → no fill, no flag.
	require.Empty(t, tk.calls)
}

// ----------- Lighter parity: no silent IF top-up of negative collateral -----------

// TestLiquidate_DoesNotTopUpFromIF is the positive assertion that the
// partial-liquidation path NEVER pulls collateral from the Insurance
// Fund as a post-trade safety net. Lighter's
// `internal_liquidate_position.rs` only inserts a `LIQUIDATION_ORDER +
// IOC + reduce_only` and lets the matching engine settle improvements
// above zero_price; there is no analogue of a chain-level
// "absorbNegativeCollateral" sweep, and the previous implementation's
// silent transfer let the IF go arbitrarily negative without any
// balance check (and bypassed the `tryLLPAbsorb` IMR gate).
//
// To make the assertion concrete we deliberately seed the victim with
// a pre-existing negative collateral value: under the old code path
// this would have moved the deficit straight to the IF account; under
// the new code path both accounts must be left exactly as they were.
func TestLiquidate_DoesNotTopUpFromIF(t *testing.T) {
	ak := newStubAccount()
	ak.accounts[perptypes.InsuranceFundOperatorAccountIdx] = accounttypes.Account{
		AccountIndex: perptypes.InsuranceFundOperatorAccountIdx,
		AccountType:  perptypes.InsuranceFundAccountType,
		Collateral:   math.NewInt(1_000_000),
	}
	ak.accounts[100] = accounttypes.Account{
		AccountIndex: 100, Collateral: math.NewInt(-50),
	}
	ak.pos[[2]uint64{100, 0}] = accounttypes.AccountPosition{
		AccountIndex: 100, MarketIndex: 0,
		Position: math.NewInt(10), EntryQuote: math.NewInt(-100),
		LastFundingRatePrefixSum: math.ZeroInt(), AllocatedMargin: math.ZeroInt(),
	}
	rk := newStubRisk()
	rk.status = perptypes.HealthPartialLiquidation
	tk := &stubTrade{}
	matchk := newStubMatching()
	k, ctx := newKeeper(t, ak, rk, tk, matchk)

	require.NoError(t, k.Liquidate(ctx, 100, 0, 5))

	// Matching IOC must have been driven, but the IF account's
	// collateral and the victim's pre-existing deficit must both be
	// untouched: there is no post-trade collateral movement in
	// Lighter's partial-liquidation path.
	require.Len(t, matchk.liqCalls, 1, "partial liq must drive matching IOC exactly once")
	require.True(t,
		ak.accounts[perptypes.InsuranceFundOperatorAccountIdx].Collateral.Equal(math.NewInt(1_000_000)),
		"IF collateral must not be debited as a post-trade top-up (got=%s)",
		ak.accounts[perptypes.InsuranceFundOperatorAccountIdx].Collateral.String(),
	)
	require.True(t, ak.accounts[100].Collateral.Equal(math.NewInt(-50)),
		"victim's pre-existing negative collateral must persist (got=%s)",
		ak.accounts[100].Collateral.String(),
	)
}

// TestDeleverage_LeavesResidualOnVictim covers the FULL/BANKRUPTCY
// arm: even though the LLP / IF participates as the deleverage
// counterparty, any residual negative collateral that may exist on
// the victim's ledger after the trade settles must NOT be silently
// transferred to the IF. Lighter's `internal_deleverage.rs` settles
// at `zero_quote` and lets the bankrupt's ledger reflect the truth;
// it has no equivalent of a post-block IF top-up sweep.
func TestDeleverage_LeavesResidualOnVictim(t *testing.T) {
	ak := newStubAccount()
	ak.accounts[perptypes.InsuranceFundOperatorAccountIdx] = accounttypes.Account{
		AccountIndex: perptypes.InsuranceFundOperatorAccountIdx,
		AccountType:  perptypes.InsuranceFundAccountType,
		Collateral:   math.NewInt(1_000_000),
		PublicPoolInfo: &accounttypes.PublicPoolInfo{
			Status:         perptypes.PublicPoolStatusActive,
			TotalShares:    math.NewInt(1),
			OperatorShares: math.NewInt(1),
		},
	}
	// Bankrupt with a residual debt (-75) plus enough remaining
	// collateral to absorb the close-out's predicted realised PnL
	// at zeroPrice=10 (the stubRisk default). EntryQuote uses the
	// production canonical sign so `ApplyFill` returns a consistent
	// value with the engine.
	ak.accounts[100] = accounttypes.Account{
		AccountIndex: 100, Collateral: math.NewInt(-75),
	}
	ak.pos[[2]uint64{100, 0}] = accounttypes.AccountPosition{
		AccountIndex: 100, MarketIndex: 0,
		Position: math.NewInt(20), EntryQuote: math.NewInt(2_000),
		LastFundingRatePrefixSum: math.ZeroInt(), AllocatedMargin: math.ZeroInt(),
	}
	rk := newStubRisk()
	rk.status = perptypes.HealthFullLiquidation
	tk := &stubTrade{}
	matchk := newStubMatching()
	k, ctx := newKeeper(t, ak, rk, tk, matchk)

	require.NoError(t, k.Deleverage(ctx, 100, 0, perptypes.InsuranceFundOperatorAccountIdx, 20))

	// Exactly one fill (LLP as taker, victim as maker), and neither
	// side's ledger value moves outside of what ApplyPerpsMatching
	// itself would have done — the stub trade engine does not touch
	// collateral, so any post-trade collateral mutation here would
	// have come from a `absorbNegativeCollateral` sweep that no
	// longer exists.
	require.Len(t, tk.calls, 1)
	require.Equal(t, perptypes.InsuranceFundOperatorAccountIdx, tk.calls[0].TakerAccountIndex)
	require.True(t, ak.accounts[100].Collateral.Equal(math.NewInt(-75)),
		"victim residual collateral must persist (got=%s)",
		ak.accounts[100].Collateral.String(),
	)
	require.True(t,
		ak.accounts[perptypes.InsuranceFundOperatorAccountIdx].Collateral.Equal(math.NewInt(1_000_000)),
		"IF collateral must not be debited beyond the trade itself (got=%s)",
		ak.accounts[perptypes.InsuranceFundOperatorAccountIdx].Collateral.String(),
	)
}

// TestEndBlocker_BankruptResidueStaysWithVictim covers the worst-case
// path: a bankrupt account whose LLP takeover would breach the IF's
// IMR AND whose ADL queue is empty (no profitable opposite-side
// counterparties). Under Lighter's design the position simply remains
// open and is re-evaluated next block; there is no chain-level rescue
// that drains the IF to make the bankrupt's collateral non-negative.
//
// Pre-fix behaviour: the EndBlocker would silently move the residual
// negative collateral to the IF (which itself has no balance check)
// regardless of the LLP IMR gate's verdict, completely defeating the
// LLP→ADL waterfall. Post-fix: ledger values are untouched.
func TestEndBlocker_BankruptResidueStaysWithVictim(t *testing.T) {
	ak := newStubAccount()
	// IF that would breach IMR if it took over the position.
	ak.accounts[perptypes.InsuranceFundOperatorAccountIdx] = accounttypes.Account{
		AccountIndex: perptypes.InsuranceFundOperatorAccountIdx,
		AccountType:  perptypes.InsuranceFundAccountType,
		Collateral:   math.NewInt(100),
		PublicPoolInfo: &accounttypes.PublicPoolInfo{
			Status: perptypes.PublicPoolStatusActive,
		},
	}
	// Bankrupt victim with deeply negative collateral and no ADL
	// counterparty at all — autoADL must walk an empty queue.
	ak.accounts[100] = accounttypes.Account{
		AccountIndex: 100, Collateral: math.NewInt(-200),
	}
	ak.pos[[2]uint64{100, 0}] = accounttypes.AccountPosition{
		AccountIndex: 100, MarketIndex: 0,
		Position: math.NewInt(50), EntryQuote: math.NewInt(-10_000),
		LastFundingRatePrefixSum: math.ZeroInt(), AllocatedMargin: math.ZeroInt(),
	}
	rk := newStubRisk()
	rk.status = perptypes.HealthBankruptcy
	rk.zero[[2]uint64{100, 0}] = 100
	// Single victim position; ranking is trivial. At default
	// mark=100 the (50, -10000) long realises uPnL = +15000 (offset
	// by entry sign convention), but only the sign matters for the
	// "LLP first" preflight here.
	rk.postSim[perptypes.InsuranceFundOperatorAccountIdx] = risktypes.RiskParameters{
		Collateral:                   math.NewInt(100),
		TotalAccountValue:            math.NewInt(50),
		InitialMarginRequirement:     math.NewInt(500), // breaches
		MaintenanceMarginRequirement: math.NewInt(250),
		CloseOutMarginRequirement:    math.NewInt(125),
	}
	tk := &stubTrade{}
	matchk := newStubMatching()
	k, ctx := newKeeper(t, ak, rk, tk, matchk)

	require.NoError(t, k.EndBlocker(ctx))

	// LLP refused (IMR breach) and ADL queue is empty: no fill
	// should have been issued at all.
	require.Empty(t, tk.calls,
		"no fill expected when LLP rejects and ADL queue is empty")
	// Both ledgers must be exactly as they started — Lighter does
	// not silently top up bankruptcy losses out of the IF.
	require.True(t, ak.accounts[100].Collateral.Equal(math.NewInt(-200)),
		"victim residual debt must persist (got=%s)",
		ak.accounts[100].Collateral.String(),
	)
	require.True(t,
		ak.accounts[perptypes.InsuranceFundOperatorAccountIdx].Collateral.Equal(math.NewInt(100)),
		"IF collateral must not be debited as a post-block sweep (got=%s)",
		ak.accounts[perptypes.InsuranceFundOperatorAccountIdx].Collateral.String(),
	)
}

// TestDeleverage_BankruptRiskRegressionRejected covers Gap B: when the
// bankrupt's post-trade IsValidRiskChange rejects (e.g., a pricing
// pathology that worsens TAV/MMR despite the close-out being
// supposedly improving), the entire deleverage trade is aborted —
// previously perpdex skipped the bankrupt check on the LLP path
// (`SkipMakerRiskCheck=true`), allowing such regressions through.
//
// The new behaviour mirrors Lighter `internal_deleverage.rs` which
// asserts `is_valid_risk_change` on bankrupt regardless of the
// deleverager type.
func TestDeleverage_BankruptRiskRegressionRejected(t *testing.T) {
	ak := newStubAccount()
	ak.accounts[perptypes.InsuranceFundOperatorAccountIdx] = accounttypes.Account{
		AccountIndex: perptypes.InsuranceFundOperatorAccountIdx,
		AccountType:  perptypes.InsuranceFundAccountType,
		Collateral:   math.NewInt(10_000_000),
		PublicPoolInfo: &accounttypes.PublicPoolInfo{
			Status:         perptypes.PublicPoolStatusActive,
			TotalShares:    math.NewInt(1),
			OperatorShares: math.NewInt(1),
		},
	}
	ak.accounts[100] = accounttypes.Account{AccountIndex: 100, Collateral: math.NewInt(10_000)}
	ak.pos[[2]uint64{100, 0}] = accounttypes.AccountPosition{
		AccountIndex: 100, MarketIndex: 0,
		Position: math.NewInt(50), EntryQuote: math.NewInt(5_000),
		LastFundingRatePrefixSum: math.ZeroInt(), AllocatedMargin: math.ZeroInt(),
	}
	rk := newStubRisk()
	rk.status = perptypes.HealthFullLiquidation
	tk := &stubTrade{
		// Simulate post-trade risk regression on the bankrupt side
		// so the engine returns ErrMakerRiskRegression. Pre-fix the
		// LLP path used SkipMakerRiskCheck=true and would have silently
		// committed.
		err: liqtypes.ErrInsuranceUnderfunded.Wrap("simulated bankrupt risk regression"),
	}
	matchk := newStubMatching()
	k, ctx := newKeeper(t, ak, rk, tk, matchk)

	err := k.Deleverage(ctx, 100, 0, perptypes.InsuranceFundOperatorAccountIdx, 50)
	require.Error(t, err,
		"bankrupt-side post-trade risk regression must abort the deleverage tx")

	// The flag-controlled checks on the engine call should have
	// requested bankrupt validation (SkipMakerRiskCheck=false) and
	// skipped the LLP/IF taker side (SkipTakerRiskCheck=true).
	require.Len(t, tk.calls, 1)
	require.False(t, tk.calls[0].SkipMakerRiskCheck,
		"bankrupt (maker) post-trade risk check must remain enabled in deleverage path")
	require.True(t, tk.calls[0].SkipTakerRiskCheck,
		"LLP / IF deleverager (taker) skips post-trade risk check")
}

// TestDeleverage_InsufficientDeleveragerCollateral_UserADL covers Gap C
// deleverager branch: under user-ADL the deleverager's own collateral
// is also asserted (perpdex defense-in-depth + Lighter parity for
// `is_deleverager_has_enough_cross_collateral`). Insufficient
// collateral on the user-ADL deleverager rejects the trade.
//
// IF / pool deleveragers are NOT subject to this assert; that case is
// covered by the absence of an `ErrInsufficientCollateral` failure in
// `TestEndBlocker_FullLiquidationPrefersLLPThenADL`.
func TestDeleverage_InsufficientDeleveragerCollateral_UserADL(t *testing.T) {
	ak := newStubAccount()
	// Bankrupt with positive collateral so the bankrupt-side assert
	// passes by short-circuit.
	ak.accounts[100] = accounttypes.Account{AccountIndex: 100, Collateral: math.NewInt(1_000_000)}
	ak.pos[[2]uint64{100, 0}] = accounttypes.AccountPosition{
		AccountIndex: 100, MarketIndex: 0,
		Position: math.NewInt(50), EntryQuote: math.NewInt(5_000),
		LastFundingRatePrefixSum: math.ZeroInt(), AllocatedMargin: math.ZeroInt(),
	}
	// User-ADL deleverager: opposite-side, but no cushion at all.
	// Closing their short at zeroPrice=10 against EQ=-5_000 yields
	// realised PnL ≈ -4_500 (in the engine's "Collateral += PnL"
	// frame) which they cannot cover.
	ak.accounts[200] = accounttypes.Account{
		AccountIndex: 200, AccountType: perptypes.MasterAccountType,
		Collateral: math.NewInt(0),
	}
	ak.pos[[2]uint64{200, 0}] = accounttypes.AccountPosition{
		AccountIndex: 200, MarketIndex: 0,
		Position: math.NewInt(-50), EntryQuote: math.NewInt(-5_000),
		LastFundingRatePrefixSum: math.ZeroInt(), AllocatedMargin: math.ZeroInt(),
	}
	rk := newStubRisk()
	rk.status = perptypes.HealthFullLiquidation
	// Force a low zeroPrice (10) for the bankrupt so closing the
	// deleverager's short at that price realises ≈ -4500 in the
	// engine's "Collateral += PnL" frame (deleverager has 0 cushion).
	// Without this override the stub falls through to mark=100, at
	// which point the close PnL is zero and the assert short-circuits.
	rk.zero[[2]uint64{100, 0}] = 10
	tk := &stubTrade{}
	matchk := newStubMatching()
	k, ctx := newKeeper(t, ak, rk, tk, matchk)

	err := k.Deleverage(ctx, 100, 0, 200, 50)
	require.Error(t, err)
	require.ErrorIs(t, err, liqtypes.ErrInsufficientCollateral)
	require.Empty(t, tk.calls)
}

// TestEndBlocker_ADLCandidateInsufficientCollateral_AdvancesToNext
// covers Gap C 内 autoADL: when the first ADL candidate's collateral
// cannot cover the close-out at the candidate-specific settle price,
// autoADL must move on to the next candidate rather than aborting.
func TestEndBlocker_ADLCandidateInsufficientCollateral_AdvancesToNext(t *testing.T) {
	ak := newStubAccount()
	// IF that breaches IMR so EndBlocker delegates to autoADL.
	ak.accounts[perptypes.InsuranceFundOperatorAccountIdx] = accounttypes.Account{
		AccountIndex: perptypes.InsuranceFundOperatorAccountIdx,
		AccountType:  perptypes.InsuranceFundAccountType,
		Collateral:   math.NewInt(100),
		PublicPoolInfo: &accounttypes.PublicPoolInfo{
			Status: perptypes.PublicPoolStatusActive,
		},
	}
	// Bankrupt with sufficient cushion for ADL settle.
	ak.accounts[100] = accounttypes.Account{AccountIndex: 100, Collateral: math.NewInt(1_000)}
	ak.pos[[2]uint64{100, 0}] = accounttypes.AccountPosition{
		AccountIndex: 100, MarketIndex: 0,
		Position: math.NewInt(50), EntryQuote: math.NewInt(5_000),
		LastFundingRatePrefixSum: math.ZeroInt(), AllocatedMargin: math.ZeroInt(),
	}
	// First candidate (highest profit rank) has zero cushion and
	// will trip the deleverager-side collateral assert. Picks a
	// slightly more negative EntryQuote (-2200) than 202 (-2000) so
	// that at mark=100 its uPnL ratio (=1200/2200) exceeds 202's
	// (=1000/2000) and it ranks first in BuildADLQueue.
	ak.accounts[201] = accounttypes.Account{
		AccountIndex: 201, AccountType: perptypes.MasterAccountType,
		Collateral: math.NewInt(0),
	}
	ak.pos[[2]uint64{201, 0}] = accounttypes.AccountPosition{
		AccountIndex: 201, MarketIndex: 0,
		Position: math.NewInt(-10), EntryQuote: math.NewInt(-2_200),
		LastFundingRatePrefixSum: math.ZeroInt(), AllocatedMargin: math.ZeroInt(),
	}
	// Second candidate has a deep cushion and matches.
	ak.accounts[202] = accounttypes.Account{
		AccountIndex: 202, AccountType: perptypes.MasterAccountType,
		Collateral: math.NewInt(1_000_000),
	}
	ak.pos[[2]uint64{202, 0}] = accounttypes.AccountPosition{
		AccountIndex: 202, MarketIndex: 0,
		Position: math.NewInt(-10), EntryQuote: math.NewInt(-2_000),
		LastFundingRatePrefixSum: math.ZeroInt(), AllocatedMargin: math.ZeroInt(),
	}
	rk := newStubRisk()
	// Only the bankrupt (100) is in FULL_LIQUIDATION — the ADL
	// counterparties (201, 202) are healthy.
	rk.status = perptypes.HealthHealthy
	rk.statuses[100] = perptypes.HealthFullLiquidation
	rk.zero[[2]uint64{100, 0}] = 100
	rk.zero[[2]uint64{201, 0}] = 110
	rk.zero[[2]uint64{202, 0}] = 110
	rk.postSim[perptypes.InsuranceFundOperatorAccountIdx] = risktypes.RiskParameters{
		Collateral:                   math.NewInt(100),
		TotalAccountValue:            math.NewInt(50),
		InitialMarginRequirement:     math.NewInt(500),
		MaintenanceMarginRequirement: math.NewInt(250),
		CloseOutMarginRequirement:    math.NewInt(125),
	}
	tk := &stubTrade{}
	matchk := newStubMatching()
	k, ctx := newKeeper(t, ak, rk, tk, matchk)

	require.NoError(t, k.EndBlocker(ctx))

	require.NotEmpty(t, tk.calls,
		"second ADL candidate must take over after the first one is rejected")
	for _, f := range tk.calls {
		require.NotEqual(t, uint64(201), f.TakerAccountIndex,
			"candidate 201 had insufficient collateral; must have been skipped")
	}
	require.Equal(t, uint64(202), tk.calls[0].TakerAccountIndex)
}

// TestEndBlocker_CrossAggregateRefreshedAcrossMarkets is the regression
// test for the cross-aggregate staleness audit (PR review P1):
// processAccount must NOT carry pre-mutation cross RiskParameters or
// status across markets when the previous market's fill has just
// shifted them.
//
// Setup: account 100 holds two cross positions (markets 0 and 1) and
// is FULL_LIQUIDATION at the start of the block. The LLP can absorb
// both. After the FIRST absorption (market 0) the stubbed trade
// engine flips the account to HEALTHY — modelling the realised PnL
// having lifted TAV above IMR. With a fresh per-call snapshot the
// next market's tryLLPAbsorb sees the new status and Deleverage's
// `victimHealthForPosition` rejects with ErrNotBankrupt; with a
// cached aggregate the second fill would still go through.
func TestEndBlocker_CrossAggregateRefreshedAcrossMarkets(t *testing.T) {
	ak := newStubAccount()
	ak.accounts[perptypes.InsuranceFundOperatorAccountIdx] = accounttypes.Account{
		AccountIndex: perptypes.InsuranceFundOperatorAccountIdx,
		AccountType:  perptypes.InsuranceFundAccountType,
		Collateral:   math.NewInt(10_000_000),
		PublicPoolInfo: &accounttypes.PublicPoolInfo{
			Status:         perptypes.PublicPoolStatusActive,
			TotalShares:    math.NewInt(1),
			OperatorShares: math.NewInt(1),
		},
	}
	ak.accounts[100] = accounttypes.Account{AccountIndex: 100, Collateral: math.NewInt(10)}
	// Two FULL_LIQUIDATION cross positions. Market 0 is the worst
	// (uPnL = pos*mark - EQ = 50*100 - 10_000 = -5_000 at mark=100);
	// market 1 is less bad (uPnL = 5*100 - 1_000 = -500). The
	// LLP-takeover ranking and the persisted-position iterator both
	// process market 0 first, so the post-fill mutation we install
	// below fires before market 1 is reached.
	ak.pos[[2]uint64{100, 0}] = accounttypes.AccountPosition{
		AccountIndex: 100, MarketIndex: 0,
		Position: math.NewInt(50), EntryQuote: math.NewInt(10_000),
		LastFundingRatePrefixSum: math.ZeroInt(), AllocatedMargin: math.ZeroInt(),
	}
	ak.pos[[2]uint64{100, 1}] = accounttypes.AccountPosition{
		AccountIndex: 100, MarketIndex: 1,
		Position: math.NewInt(5), EntryQuote: math.NewInt(1_000),
		LastFundingRatePrefixSum: math.ZeroInt(), AllocatedMargin: math.ZeroInt(),
	}
	rk := newStubRisk()
	rk.statuses[100] = perptypes.HealthFullLiquidation

	// Simulate the cross account flipping HEALTHY after the first
	// absorption. A keeper that cached crossRP/status from the start
	// of processAccount would still issue a second fill against the
	// (no longer bankrupt) account.
	tk := &stubTrade{
		onCall: func(f tradekeeper.PerpFill) {
			if f.MarketIndex == 0 {
				rk.statuses[100] = perptypes.HealthHealthy
				rk.cross[100] = riskParamsForStatus(perptypes.HealthHealthy)
			}
		},
	}
	matchk := newStubMatching()
	k, ctx := newKeeper(t, ak, rk, tk, matchk)

	require.NoError(t, k.EndBlocker(ctx))

	require.Len(t, tk.calls, 1,
		"only market 0 should fill: market 1 must observe the post-mutation HEALTHY status and skip")
	require.Equal(t, uint32(0), tk.calls[0].MarketIndex)
	require.GreaterOrEqual(t, rk.snapshotCalls, 2,
		"each per-market LLP/ADL invocation must build its own fresh risk snapshot")
}
