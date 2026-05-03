// Package e2e contains the integration test suite for the PerpDEX L1 chain.
//
// The suite boots a full in-process PerpDEXApp via [app/helpers.Setup], which
// includes a single bonded validator and one delegator account holding `uperp`.
// On top of that we mint per-test USDC (`uusdc`) for a configurable number of
// users so each scenario can immediately deposit collateral, place orders and
// drive cross-module flows without crafting validator transactions.
//
// Tests in this package follow the testify suite pattern modelled on
// `cosmos/gaia/tests/e2e`: they live in files named `e2e_<scenario>_test.go`
// and are receiver methods on [PerpdexTestSuite]. Inside a method, helpers
// from sibling packages (`tests/e2e/msg` and `tests/e2e/query`) are exposed
// through the embedded helper structs to keep call-sites short.
//
// Design note: governance-only Msgs are dispatched directly via the relevant
// MsgServer with `Authority = govModuleAddress`, side-stepping the gov
// proposal flow. This is the conventional shortcut used by Cosmos SDK module
// integration tests; it lets a single test exercise the same code paths that
// run when a real governance proposal succeeds, without the multi-block
// voting period.
package e2e

import (
	"testing"
	"time"

	"github.com/stretchr/testify/suite"

	abci "github.com/cometbft/cometbft/abci/types"
	tmproto "github.com/cometbft/cometbft/proto/tendermint/types"

	"cosmossdk.io/math"

	sdk "github.com/cosmos/cosmos-sdk/types"
	authtypes "github.com/cosmos/cosmos-sdk/x/auth/types"
	govtypes "github.com/cosmos/cosmos-sdk/x/gov/types"
	minttypes "github.com/cosmos/cosmos-sdk/x/mint/types"

	perp "github.com/perpdex/perpdex-l1/app"
	"github.com/perpdex/perpdex-l1/app/helpers"
	"github.com/perpdex/perpdex-l1/tests/e2e/common"
)

// DefaultNumUsers is how many TestUsers SetupTest creates by default. Tests
// that need more can override `NumUsers` in their own SetupTest before
// chaining to the embedded suite's SetupTest.
const DefaultNumUsers = 4

// TestUser is re-exported from `tests/e2e/common` so test scenarios can
// keep using `e2e.TestUser` while the helper sub-packages depend only on
// the leaf-level common package (avoiding any import cycles).
type TestUser = common.TestUser

// PerpdexTestSuite is the root testify suite shared by every Phase-13
// scenario. Each test method gets a freshly booted PerpDEXApp via SetupTest;
// state never leaks between tests.
type PerpdexTestSuite struct {
	suite.Suite

	App        *perp.PerpDEXApp
	Ctx        sdk.Context
	GovAddress sdk.AccAddress

	// NumUsers can be customised in a sub-suite SetupTest before calling
	// PerpdexTestSuite.SetupTest. Defaults to DefaultNumUsers.
	NumUsers int
	Users    []TestUser

	// blockTime is the wall-clock currently associated with App's last
	// committed block; AdvanceBlock increments it.
	blockTime time.Time
}

// SetupTest boots a fresh PerpDEXApp, mints `DefaultUSDCBalance` uusdc to
// each of `NumUsers` freshly generated test addresses, and refreshes the
// finalize-block context.
func (s *PerpdexTestSuite) SetupTest() {
	if s.NumUsers == 0 {
		s.NumUsers = DefaultNumUsers
	}

	s.App = helpers.Setup(s.T())
	// helpers.Setup runs InitChain + a single FinalizeBlock but does NOT
	// call Commit. That leaves a non-nil `finalizeBlockState` on the
	// BaseApp; subsequent FinalizeBlock calls would re-use it instead of
	// observing our mint / Msg writes performed via NewUncachedContext.
	// Force a Commit here so the next FinalizeBlock starts with a fresh
	// cache rooted at the current working tree.
	_, err := s.App.Commit()
	s.Require().NoError(err)

	s.GovAddress = authtypes.NewModuleAddress(govtypes.ModuleName)
	s.Ctx = s.App.NewUncachedContext(false, tmproto.Header{Height: s.App.LastBlockHeight()})
	// helpers.Setup advances one block with default header time (zero); we
	// pin the suite's notion of block time to the unix epoch + 1h so the
	// timestamps recorded into perpdex state are non-zero and easier to
	// reason about than `time.Time{}`.
	s.blockTime = time.Unix(0, 0).UTC().Add(time.Hour)

	s.Users = common.NewUsers(s.NumUsers)
	coins := sdk.NewCoins(sdk.NewCoin(common.USDCDenom, math.NewIntFromUint64(common.DefaultUSDCBalance)))
	for i := range s.Users {
		// Mint USDC straight to the user's bank address through the mint
		// module account (the only account in the maccPerms map with the
		// Minter permission besides ibctransfer).
		s.Require().NoError(s.App.BankKeeper.MintCoins(s.Ctx, minttypes.ModuleName, coins))
		s.Require().NoError(s.App.BankKeeper.SendCoinsFromModuleToAccount(
			s.Ctx, minttypes.ModuleName, s.Users[i].Address, coins,
		))
	}
	// Persist the funded balances so subsequent FinalizeBlock calls observe
	// them. We do this by advancing one no-op block.
	s.AdvanceBlock()
}

// User returns the i-th test user; helpful when scenarios need to pass a
// specific signer to the msg helpers.
func (s *PerpdexTestSuite) User(i int) *TestUser {
	s.Require().Less(i, len(s.Users), "user index out of range")
	return &s.Users[i]
}

// AdvanceBlock finalises the current block (running every BeginBlocker /
// EndBlocker), then refreshes s.Ctx so the next set of helper calls observes
// the post-EndBlock state. The block-time advances by [common.DefaultBlockStep].
func (s *PerpdexTestSuite) AdvanceBlock() {
	s.AdvanceBlockBy(common.DefaultBlockStep)
}

// AdvanceBlockBy is the time-aware variant of AdvanceBlock, useful when a
// test needs to land at an exact integer-hour boundary (funding) or just
// past a market expiry timestamp.
func (s *PerpdexTestSuite) AdvanceBlockBy(d time.Duration) {
	s.blockTime = s.blockTime.Add(d)
	_, err := s.App.FinalizeBlock(&abci.RequestFinalizeBlock{
		Height: s.App.LastBlockHeight() + 1,
		Time:   s.blockTime,
		Hash:   s.App.LastCommitID().Hash,
	})
	s.Require().NoError(err)
	_, err = s.App.Commit()
	s.Require().NoError(err)

	header := tmproto.Header{Height: s.App.LastBlockHeight(), Time: s.blockTime}
	s.Ctx = s.App.NewUncachedContext(false, header).WithBlockTime(s.blockTime)
}

// AdvanceBlocksBy advances `n` blocks each spaced by `d`. Equivalent to
// calling AdvanceBlockBy in a loop, but lighter on test boilerplate.
func (s *PerpdexTestSuite) AdvanceBlocksBy(n int, d time.Duration) {
	for i := 0; i < n; i++ {
		s.AdvanceBlockBy(d)
	}
}

// BlockTime returns the wall-clock that AdvanceBlock used for the most
// recent FinalizeBlock invocation.
func (s *PerpdexTestSuite) BlockTime() time.Time { return s.blockTime }

// Context returns the current writable sdk.Context backed by the same
// multistore as App. Tests should not store this value across an
// AdvanceBlock call; use s.Ctx (refreshed automatically) instead.
func (s *PerpdexTestSuite) Context() sdk.Context { return s.Ctx }

// RunSuite is the canonical wrapper used by every `e2e_*_test.go` file:
//
//	func TestE2EFoo(t *testing.T) { e2e.RunSuite(t, new(MyFooSuite)) }
//
// keeping the test entrypoint a one-liner.
func RunSuite(t *testing.T, s suite.TestingSuite) {
	t.Helper()
	suite.Run(t, s)
}
