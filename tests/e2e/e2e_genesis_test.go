package e2e_test

import (
	"testing"

	perptypes "github.com/perpdex/perpdex-l1/types"
	"github.com/perpdex/perpdex-l1/tests/e2e"
)

// GenesisSuite runs the smallest set of assertions that proves the wiring
// performed in `app/keepers` and the Phase-12 default genesis values are
// consistent with the rest of the perpdex modules. It deliberately uses
// only the suite's query shims (no Msg dispatching) so that any wiring
// regression surfaces here before the more elaborate scenarios run.
type GenesisSuite struct {
	e2e.PerpdexTestSuite
}

func TestGenesisSuite(t *testing.T) {
	e2e.RunSuite(t, new(GenesisSuite))
}

// TestUSDCSeeded confirms the asset module's default genesis registered
// USDC at the canonical asset_index (3) with margin enabled.
func (s *GenesisSuite) TestUSDCSeeded() {
	usdc := s.QueryAsset(perptypes.USDCAssetIndex)
	s.Require().Equal("uusdc", usdc.Denom)
	s.Require().True(usdc.Enabled)
	s.Require().Equal(perptypes.MarginModeEnabled, usdc.MarginMode)
}

// TestSpecialAccountsExist confirms TREASURY (idx=0) and INSURANCE_FUND
// (idx=1) module accounts were pre-created by the account module's
// default genesis.
func (s *GenesisSuite) TestSpecialAccountsExist() {
	treasury := s.QueryAccount(perptypes.TreasuryAccountIndex)
	s.Require().Equal(perptypes.MasterAccountType, treasury.AccountType)

	insurance := s.QueryAccount(perptypes.InsuranceFundOperatorAccountIdx)
	s.Require().Equal(perptypes.InsuranceFundAccountType, insurance.AccountType)
}

// TestOracleWhitelistByDefault confirms the oracle starts in WHITELIST
// mode (cold-start friendly); governance must explicitly switch to
// PoS_MEDIAN with MsgSetAggregationMode for the vote-extension code path.
func (s *GenesisSuite) TestOracleWhitelistByDefault() {
	params := s.QueryOracleParams()
	s.Require().Equal(perptypes.OracleAggWhitelist, params.AggregationMode,
		"default oracle mode must be WHITELIST until governance flips it")
	s.Require().False(params.VoteExtensionEnabled,
		"vote extensions must be disabled by default to keep the cold start safe")
}
