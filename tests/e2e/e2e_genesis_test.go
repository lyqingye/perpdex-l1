package e2e_test

import (
	"testing"

	"github.com/perpdex/perpdex-l1/tests/e2e"
	perptypes "github.com/perpdex/perpdex-l1/types"
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

// TestOracleParamsDefault confirms the oracle module ships with the
// dydx/Slinky-style ABCI++ vote-extension pipeline switched on. Risk /
// liquidation / funding all rely on `MaxAgeMs` to refuse stale prices,
// so the default value must be non-zero too.
func (s *GenesisSuite) TestOracleParamsDefault() {
	params := s.QueryOracleParams()
	s.Require().True(params.VoteExtensionEnabled,
		"vote extensions must default on so the oracle pipeline activates without governance intervention")
	s.Require().Greater(params.MaxAgeMs, int64(0),
		"max_age_ms must be positive so freshness checks can refuse stale prices")
}
