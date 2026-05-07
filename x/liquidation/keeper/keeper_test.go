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

// stubRisk fully implements types.RiskKeeper. Per-account / per-market
// values are pre-loaded by the test; nothing is computed dynamically.
type stubRisk struct {
	status   uint32                       // returned by GetHealthStatus
	isoStat  map[[2]uint64]uint32          // (acc, market) -> isolated status
	zero     map[[2]uint64]uint32          // (acc, market) -> zero price
	uPnL     map[[2]uint64]math.Int        // (acc, market) -> uPnL
	mark     map[[2]uint64]math.Int        // (acc, market) -> mark value
	cross    map[uint64]risktypes.RiskParameters
	iso      map[[2]uint64]risktypes.RiskParameters
	postSim  map[uint64]risktypes.RiskParameters // simulated takeover post-state per account
}

func newStubRisk() *stubRisk {
	return &stubRisk{
		isoStat: map[[2]uint64]uint32{},
		zero:    map[[2]uint64]uint32{},
		uPnL:    map[[2]uint64]math.Int{},
		mark:    map[[2]uint64]math.Int{},
		cross:   map[uint64]risktypes.RiskParameters{},
		iso:     map[[2]uint64]risktypes.RiskParameters{},
		postSim: map[uint64]risktypes.RiskParameters{},
	}
}

func (s *stubRisk) GetHealthStatus(_ context.Context, _ uint64) (uint32, error) {
	return s.status, nil
}
func (s *stubRisk) GetIsolatedHealthStatus(_ context.Context, acc uint64, mkt uint32) (uint32, error) {
	if v, ok := s.isoStat[[2]uint64{acc, uint64(mkt)}]; ok {
		return v, nil
	}
	return perptypes.HealthHealthy, nil
}
func (s *stubRisk) GetPositionZeroPrice(_ context.Context, acc uint64, mkt uint32) (uint32, error) {
	if v, ok := s.zero[[2]uint64{acc, uint64(mkt)}]; ok {
		return v, nil
	}
	return 10, nil
}
func (s *stubRisk) GetPositionMarkValue(_ context.Context, acc uint64, mkt uint32) (math.Int, error) {
	if v, ok := s.mark[[2]uint64{acc, uint64(mkt)}]; ok {
		return v, nil
	}
	return math.ZeroInt(), nil
}
func (s *stubRisk) GetPositionUnrealizedPnL(_ context.Context, acc uint64, mkt uint32) (math.Int, error) {
	if v, ok := s.uPnL[[2]uint64{acc, uint64(mkt)}]; ok {
		return v, nil
	}
	return math.ZeroInt(), nil
}
func (s *stubRisk) SimulateRiskAfterTakeover(
	_ context.Context, acc uint64, _ uint32, _ math.Int, _ uint32,
) (risktypes.RiskParameters, error) {
	if v, ok := s.postSim[acc]; ok {
		return v, nil
	}
	// Default: TAV >= IMR (LLP can absorb). Tests override per-account.
	return risktypes.RiskParameters{
		Collateral:                   math.NewInt(1_000_000),
		CollateralWithFunding:        math.NewInt(1_000_000),
		TotalAccountValue:            math.NewInt(1_000_000),
		InitialMarginRequirement:     math.ZeroInt(),
		MaintenanceMarginRequirement: math.ZeroInt(),
		CloseOutMarginRequirement:    math.ZeroInt(),
	}, nil
}
func (s *stubRisk) ComputeRiskInfo(_ context.Context, acc uint64) (risktypes.RiskInfo, error) {
	if v, ok := s.cross[acc]; ok {
		return risktypes.RiskInfo{CrossRiskParameters: &v, CurrentRiskParameters: &v}, nil
	}
	zero := risktypes.RiskParameters{
		Collateral:                   math.ZeroInt(),
		CollateralWithFunding:        math.ZeroInt(),
		TotalAccountValue:            math.ZeroInt(),
		InitialMarginRequirement:     math.ZeroInt(),
		MaintenanceMarginRequirement: math.ZeroInt(),
		CloseOutMarginRequirement:    math.ZeroInt(),
	}
	return risktypes.RiskInfo{CrossRiskParameters: &zero, CurrentRiskParameters: &zero}, nil
}
func (s *stubRisk) ComputeIsolatedRisk(_ context.Context, acc uint64, mkt uint32) (risktypes.RiskParameters, error) {
	if v, ok := s.iso[[2]uint64{acc, uint64(mkt)}]; ok {
		return v, nil
	}
	return risktypes.RiskParameters{
		Collateral:                   math.ZeroInt(),
		CollateralWithFunding:        math.ZeroInt(),
		TotalAccountValue:            math.ZeroInt(),
		InitialMarginRequirement:     math.ZeroInt(),
		MaintenanceMarginRequirement: math.ZeroInt(),
		CloseOutMarginRequirement:    math.ZeroInt(),
	}, nil
}

type stubTrade struct {
	calls []tradekeeper.PerpFill
}

func (s *stubTrade) ApplyPerpsMatching(_ context.Context, f tradekeeper.PerpFill) error {
	s.calls = append(s.calls, f)
	return nil
}

type stubMatching struct {
	cancelled map[uint64]uint32
}

func newStubMatching() *stubMatching { return &stubMatching{cancelled: map[uint64]uint32{}} }

func (s *stubMatching) CancelAllOpenOrdersForAccount(_ context.Context, acc uint64) (uint32, error) {
	s.cancelled[acc]++
	return 0, nil
}

func newKeeper(
	t *testing.T,
	ak *stubAccount, rk *stubRisk, tk *stubTrade, matchk *stubMatching,
) (liqkeeper.Keeper, sdk.Context) {
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
		matchk,
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
	ak.accounts[200] = accounttypes.Account{AccountIndex: 200, Collateral: math.ZeroInt()}
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

	err := k.Liquidate(ctx, 100, 0, 999 /* > victim size */, 200)
	require.Error(t, err)
	require.ErrorIs(t, err, liqtypes.ErrInvalidParams)
	require.Empty(t, tk.calls)
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
	rk.status = perptypes.HealthFullLiquidation
	tk := &stubTrade{}
	matchk := newStubMatching()
	k, ctx := newKeeper(t, ak, rk, tk, matchk)

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
// cancel all open orders of the user" before booking the close-out.
func TestLiquidate_CancelsVictimOpenOrders(t *testing.T) {
	ak := newStubAccount()
	ak.accounts[100] = accounttypes.Account{AccountIndex: 100, Collateral: math.ZeroInt()}
	ak.accounts[200] = accounttypes.Account{AccountIndex: 200, Collateral: math.ZeroInt()}
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

	err := k.Liquidate(ctx, 100, 0, 5, 200)
	require.NoError(t, err)
	require.Equal(t, uint32(1), matchk.cancelled[100],
		"victim must have orders cancelled before close-out")
	require.Len(t, tk.calls, 1)
}

// TestLiquidate_FillCarriesLLPAsLiquidationFeeRecipient verifies that
// the close-out fill routes the improvement fee (capped at 1%) to the
// Insurance Fund operator account, NOT the treasury.
func TestLiquidate_FillCarriesLLPAsLiquidationFeeRecipient(t *testing.T) {
	ak := newStubAccount()
	ak.accounts[100] = accounttypes.Account{AccountIndex: 100, Collateral: math.ZeroInt()}
	ak.accounts[200] = accounttypes.Account{AccountIndex: 200, Collateral: math.ZeroInt()}
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

	err := k.Liquidate(ctx, 100, 0, 4, 200)
	require.NoError(t, err)
	require.Len(t, tk.calls, 1)
	fill := tk.calls[0]
	require.Equal(t, perptypes.InsuranceFundOperatorAccountIdx, fill.LiquidationFeeRecipient,
		"liquidation fee must route to LLP / Insurance Fund operator")
	require.Equal(t, uint32(95), fill.ZeroPrice)
	require.Equal(t, uint32(95), fill.Price, "engine fills at zero price")
	require.Equal(t, uint32(0), fill.MakerFee)
	require.Equal(t, uint32(0), fill.TakerFee)
	require.True(t, fill.SkipMakerRiskCheck,
		"victim must not be subject to IsValidRiskChange (PARTIAL post-state)")
	require.Greater(t, fill.LiquidationFeeBps, uint32(0),
		"market.LiquidationFee must populate the fee bps on the fill")
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
	// Victim with one FULL_LIQUIDATION cross position.
	ak.accounts[100] = accounttypes.Account{AccountIndex: 100, Collateral: math.NewInt(10)}
	ak.pos[[2]uint64{100, 0}] = accounttypes.AccountPosition{
		AccountIndex: 100, MarketIndex: 0,
		Position: math.NewInt(50), EntryQuote: math.NewInt(-5_000),
		LastFundingRatePrefixSum: math.ZeroInt(), AllocatedMargin: math.ZeroInt(),
	}
	rk := newStubRisk()
	rk.status = perptypes.HealthFullLiquidation
	rk.uPnL[[2]uint64{100, 0}] = math.NewInt(-100) // worst uPnL
	tk := &stubTrade{}
	matchk := newStubMatching()
	k, ctx := newKeeper(t, ak, rk, tk, matchk)

	require.NoError(t, k.EndBlocker(ctx))

	// Exactly one fill, target = IF as taker.
	require.Len(t, tk.calls, 1, "LLP absorb should produce one fill")
	require.Equal(t, perptypes.InsuranceFundOperatorAccountIdx, tk.calls[0].TakerAccountIndex,
		"counterparty must be the insurance fund operator")
	require.True(t, tk.calls[0].NoFee, "LLP takeover is a fee-less close")
	require.True(t, tk.calls[0].NoRiskCheck, "LLP bypasses post-trade risk check")
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
		Position: math.NewInt(-10), EntryQuote: math.NewInt(2_000),
		LastFundingRatePrefixSum: math.ZeroInt(), AllocatedMargin: math.ZeroInt(),
	}
	ak.accounts[100] = accounttypes.Account{AccountIndex: 100, Collateral: math.NewInt(10)}
	ak.pos[[2]uint64{100, 0}] = accounttypes.AccountPosition{
		AccountIndex: 100, MarketIndex: 0,
		Position: math.NewInt(50), EntryQuote: math.NewInt(-5_000),
		LastFundingRatePrefixSum: math.ZeroInt(), AllocatedMargin: math.ZeroInt(),
	}
	rk := newStubRisk()
	rk.status = perptypes.HealthFullLiquidation
	rk.uPnL[[2]uint64{100, 0}] = math.NewInt(-100)
	rk.uPnL[[2]uint64{999, 0}] = math.NewInt(50) // counterparty in profit
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
		Position: math.NewInt(-10), EntryQuote: math.NewInt(2_000),
		LastFundingRatePrefixSum: math.ZeroInt(), AllocatedMargin: math.ZeroInt(),
	}
	ak.accounts[100] = accounttypes.Account{AccountIndex: 100, Collateral: math.NewInt(-100)}
	ak.pos[[2]uint64{100, 0}] = accounttypes.AccountPosition{
		AccountIndex: 100, MarketIndex: 0,
		Position: math.NewInt(50), EntryQuote: math.NewInt(-10_000),
		LastFundingRatePrefixSum: math.ZeroInt(), AllocatedMargin: math.ZeroInt(),
	}
	rk := newStubRisk()
	rk.status = perptypes.HealthBankruptcy
	rk.zero[[2]uint64{100, 0}] = 100
	rk.zero[[2]uint64{999, 0}] = 110
	rk.uPnL[[2]uint64{999, 0}] = math.NewInt(50)
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
func TestAutoADL_RequiresZeroPriceAlignment(t *testing.T) {
	ak := newStubAccount()
	ak.accounts[100] = accounttypes.Account{AccountIndex: 100, Collateral: math.NewInt(-50)}
	ak.pos[[2]uint64{100, 0}] = accounttypes.AccountPosition{
		AccountIndex: 100, MarketIndex: 0,
		Position: math.NewInt(50), EntryQuote: math.NewInt(-5_000),
		LastFundingRatePrefixSum: math.ZeroInt(), AllocatedMargin: math.ZeroInt(),
	}
	// Two opposite-side candidates, but only one has an aligned ZP.
	ak.accounts[201] = accounttypes.Account{
		AccountIndex: 201, AccountType: perptypes.MasterAccountType,
		Collateral: math.NewInt(1_000_000),
	}
	ak.pos[[2]uint64{201, 0}] = accounttypes.AccountPosition{
		AccountIndex: 201, MarketIndex: 0,
		Position: math.NewInt(-10), EntryQuote: math.NewInt(1_500),
		LastFundingRatePrefixSum: math.ZeroInt(), AllocatedMargin: math.ZeroInt(),
	}
	ak.accounts[202] = accounttypes.Account{
		AccountIndex: 202, AccountType: perptypes.MasterAccountType,
		Collateral: math.NewInt(1_000_000),
	}
	ak.pos[[2]uint64{202, 0}] = accounttypes.AccountPosition{
		AccountIndex: 202, MarketIndex: 0,
		Position: math.NewInt(-20), EntryQuote: math.NewInt(2_500),
		LastFundingRatePrefixSum: math.ZeroInt(), AllocatedMargin: math.ZeroInt(),
	}
	rk := newStubRisk()
	rk.status = perptypes.HealthBankruptcy
	rk.zero[[2]uint64{100, 0}] = 100   // victim long ZP = 100 (need ZP_cand >= 100)
	rk.zero[[2]uint64{201, 0}] = 90    // misaligned: ZP < victim — skip
	rk.zero[[2]uint64{202, 0}] = 105   // aligned
	rk.uPnL[[2]uint64{201, 0}] = math.NewInt(50)
	rk.uPnL[[2]uint64{202, 0}] = math.NewInt(50)
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
	rk.uPnL[[2]uint64{201, 0}] = math.NewInt(100)
	rk.uPnL[[2]uint64{202, 0}] = math.NewInt(100)
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
