package e2e_test

import (
	"testing"

	"github.com/cosmos/cosmos-sdk/crypto/keys/secp256k1"
	sdk "github.com/cosmos/cosmos-sdk/types"

	perptypes "github.com/perpdex/perpdex-l1/types"
	oracletypes "github.com/perpdex/perpdex-l1/x/oracle/types"

	"github.com/perpdex/perpdex-l1/tests/e2e"
	"github.com/perpdex/perpdex-l1/tests/e2e/msg"
)

// OracleSuite covers the two oracle code paths exposed by x/oracle:
//
//  1. WHITELIST mode (default): governance whitelists an off-chain
//     provider with `MsgAddOracleProvider`; the provider then submits
//     `MsgInjectOracle` and the prices are stored verbatim.
//  2. PoS_MEDIAN mode: governance flips the aggregation mode and the
//     proposer aggregates ABCI++ vote-extension data into a single
//     `MsgAggregateOracleVotes`. Once that lands, OraclePrice records
//     should be updated and ValidatorOracleStats incremented.
//
// We drive #1 directly. For #2 we synthesise a `MsgAggregateOracleVotes`
// signed by the governance authority — that is the same path the proposer
// follows post-aggregation in a real PoS_MEDIAN run.
type OracleSuite struct {
	e2e.PerpdexTestSuite

	BTCAssetIndex uint32
	MarketIndex   uint32
}

func TestOracleSuite(t *testing.T) {
	e2e.RunSuite(t, new(OracleSuite))
}

func (s *OracleSuite) SetupTest() {
	s.PerpdexTestSuite.SetupTest()
	s.BTCAssetIndex = s.RegisterAsset(msg.AssetOpts{
		Denom:               "ubtc",
		DisplayName:         "BTC",
		Decimals:            8,
		ExtensionMultiplier: 1,
		MinTransferAmount:   1,
		MinWithdrawalAmount: 1,
		MarginMode:          perptypes.MarginModeDisabled,
	})
	s.MarketIndex = s.CreatePerpMarket(msg.DefaultPerpMarketOpts(1, s.BTCAssetIndex))
}

// TestWhitelistInjection drives the WHITELIST happy path.
func (s *OracleSuite) TestWhitelistInjection() {
	// Sanity: default genesis runs in WHITELIST mode.
	params := s.QueryOracleParams()
	s.Require().Equal(perptypes.OracleAggWhitelist, params.AggregationMode,
		"oracle must default to WHITELIST until governance flips it")

	provider := s.Users[0].Address
	s.AddOracleProvider(provider, "test-provider")

	const indexPrice = uint32(60_000)
	const markPrice = uint32(60_010)
	s.InjectPrice(provider, s.MarketIndex, indexPrice, markPrice)

	got := s.QueryOraclePrice(s.MarketIndex)
	s.Require().Equal(indexPrice, got.IndexPrice, "WHITELIST inject must persist index price")
	s.Require().Equal(markPrice, got.MarkPrice, "WHITELIST inject must persist mark price")
	s.Require().Equal(perptypes.OracleAggWhitelist, got.AggregationMethod,
		"prices written via inject must record method=WHITELIST")
	s.Require().EqualValues(1, got.ParticipantCount,
		"WHITELIST mode encodes participant count = 1")
}

// TestRejectsInjectionFromUnknownProvider verifies that a Msg posted by
// an address that has never been whitelisted is rejected. This is the
// exact same code path that prevents stray transactions from poisoning
// oracle prices in production.
func (s *OracleSuite) TestRejectsInjectionFromUnknownProvider() {
	_, err := msg.InjectPrice(s.App, s.Ctx, s.Users[1].Address, []oracletypes.MarketPrice{{
		MarketIndex: s.MarketIndex,
		IndexPrice:  50_000,
		MarkPrice:   50_000,
	}})
	s.Require().Error(err, "non-provider must not be able to inject")
}

// TestRejectsInjectionInPosMedianMode confirms that once governance has
// flipped the chain into PoS_MEDIAN mode, MsgInjectOracle is gated off
// — the only path to set prices is MsgAggregateOracleVotes.
func (s *OracleSuite) TestRejectsInjectionInPosMedianMode() {
	provider := s.Users[0].Address
	s.AddOracleProvider(provider, "test-provider")
	s.SetAggregationMode(perptypes.OracleAggPosMedian)

	_, err := msg.InjectPrice(s.App, s.Ctx, provider, []oracletypes.MarketPrice{{
		MarketIndex: s.MarketIndex,
		IndexPrice:  50_000,
		MarkPrice:   50_000,
	}})
	s.Require().Error(err, "inject must be rejected once mode flips to PoS_MEDIAN")
	s.Require().ErrorContains(err, "WHITELIST",
		"err must reference the WHITELIST-only constraint to aid debugging")
}

// TestPosMedianAggregationFlow walks the validator-aggregation path:
// governance flips the mode, then the proposer (modeled here as the gov
// authority) submits a single MsgAggregateOracleVotes with prices and
// per-validator records. We verify the stored OraclePrice records the
// PoS aggregation method and that ValidatorOracleStats was bumped.
func (s *OracleSuite) TestPosMedianAggregationFlow() {
	s.SetAggregationMode(perptypes.OracleAggPosMedian)
	s.Require().Equal(perptypes.OracleAggPosMedian, s.QueryOracleParams().AggregationMode)

	const indexPrice = uint32(70_500)
	const markPrice = uint32(70_510)

	// MsgAggregateOracleVotes does no bech32 validation on
	// ValidatorAddress (only Authority must parse), so any non-empty
	// string suffices as the lookup key for ValidatorOracleStats.
	validatorAddr := "perpdexvaloper1examplevalidator00000000000000abcdef"
	s.AggregateVotes(
		s.Ctx.BlockHeight(),
		[]oracletypes.MarketAggregation{{
			MarketIndex: s.MarketIndex,
			IndexPrice:  indexPrice,
			MarkPrice:   markPrice,
		}},
		[]oracletypes.VoterRecord{{
			ValidatorAddress:   validatorAddr,
			VotingPower:        100,
			Participated:       true,
			DeviantMarketCount: 0,
		}},
	)

	got := s.QueryOraclePrice(s.MarketIndex)
	s.Require().Equal(indexPrice, got.IndexPrice)
	s.Require().Equal(markPrice, got.MarkPrice)
	s.Require().Equal(perptypes.OracleAggPosMedian, got.AggregationMethod,
		"aggregate path must flag method=PoS_MEDIAN on the stored OraclePrice")
	s.Require().EqualValues(1, got.ParticipantCount,
		"participant count must reflect the number of voter records sent in the Msg")

	// ValidatorOracleStats must be incremented for the participant.
	stats, err := s.App.OracleKeeper.Stats.Get(s.Ctx, validatorAddr)
	s.Require().NoError(err, "stats must be initialised for any participant we record")
	s.Require().EqualValues(1, stats.TotalVotesSubmitted)
	s.Require().EqualValues(0, stats.TotalVotesMissed)
	s.Require().EqualValues(0, stats.ConsecutiveMissed)
}

// TestBindOracleOperator covers the validator -> oracle-operator binding
// happy path. MsgBindOracleOperator's ValidateBasic enforces that Sender
// is a valid bech32 *account* address (the perpdex chain uses `px` as
// the account prefix), and the keeper requires `Sender ==
// ValidatorAddress`. We therefore use a fresh AccAddress for both.
func (s *OracleSuite) TestBindOracleOperator() {
	pk := secp256k1.GenPrivKey()
	validatorAddr := sdk.AccAddress(pk.PubKey().Address()).String()
	operatorAddr := sdk.AccAddress(secp256k1.GenPrivKey().PubKey().Address()).String()

	_, err := msg.BindOracleOperator(s.App, s.Ctx, validatorAddr, operatorAddr, "v1")
	s.Require().NoError(err, "validator can bind their own oracle operator")

	binding, err := s.App.OracleKeeper.Bindings.Get(s.Ctx, validatorAddr)
	s.Require().NoError(err)
	s.Require().Equal(operatorAddr, binding.OracleOperatorAddress)
}
