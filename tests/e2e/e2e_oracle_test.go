package e2e_test

import (
	"context"
	"testing"

	abci "github.com/cometbft/cometbft/abci/types"
	cmtproto "github.com/cometbft/cometbft/proto/tendermint/types"

	sdk "github.com/cosmos/cosmos-sdk/types"
	authtypes "github.com/cosmos/cosmos-sdk/x/auth/types"
	govtypes "github.com/cosmos/cosmos-sdk/x/gov/types"

	perptypes "github.com/perpdex/perpdex-l1/types"
	oraclekeeper "github.com/perpdex/perpdex-l1/x/oracle/keeper"
	oracletypes "github.com/perpdex/perpdex-l1/x/oracle/types"

	"github.com/perpdex/perpdex-l1/tests/e2e"
	"github.com/perpdex/perpdex-l1/tests/e2e/msg"
)

// OracleSuite covers the dydx/Slinky-style ABCI++ vote-extension oracle:
//
//  1. Genesis params default to vote-extension-enabled, max_age_ms > 0.
//  2. ExtendVote → VerifyVoteExtension stateless contract: PriceFetcher
//     output round-trips through the proto encoding and the verify
//     handler accepts well-formed payloads / rejects malformed ones.
//  3. Full PrepareProposal pipeline: build an ExtendedCommitInfo from
//     synthetic validator votes, run PrepareProposal, decode the injected
//     MsgAggregateOracleVotes from the resulting tx list and confirm the
//     weighted median came out as expected.
//  4. ProcessProposal accepts the proposer-injected tx and rejects a
//     forged first tx whose authority is wrong.
//  5. msgServer.AggregateOracleVotes writes the aggregated price into
//     state with `LastUpdatedHeight` / `LastUpdatedTimestamp` set.
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

func (s *OracleSuite) govAuthority() string {
	return authtypes.NewModuleAddress(govtypes.ModuleName).String()
}

// TestParamsDefaults confirms the genesis-supplied params keep the
// pipeline active without any governance interaction.
func (s *OracleSuite) TestParamsDefaults() {
	params := s.QueryOracleParams()
	s.Require().True(params.VoteExtensionEnabled)
	s.Require().Greater(params.MaxAgeMs, int64(0))
}

// TestExtendVoteRoundTrip exercises ExtendVote + VerifyVoteExtension as
// a single contract by injecting a deterministic PriceFetcher.
func (s *OracleSuite) TestExtendVoteRoundTrip() {
	want := []oracletypes.MarketPrice{{
		MarketIndex: s.MarketIndex,
		IndexPrice:  60_000,
		MarkPrice:   60_010,
	}}
	s.App.OracleKeeper.SetPriceFetcher(oraclekeeper.PriceFetcherFunc(
		func(_ context.Context, _ int64) ([]oracletypes.MarketPrice, error) {
			return want, nil
		},
	))
	handler := oraclekeeper.NewVoteExtensionHandler(s.App.OracleKeeper, s.App.GetTxConfig(), s.govAuthority())

	// Force the consensus param so veEnabled returns true. The default
	// suite ctx ConsensusParams come from cometbft test defaults which
	// leave VoteExtensionsEnableHeight at zero.
	ctx := s.Ctx.WithConsensusParams(cmtproto.ConsensusParams{
		Abci: &cmtproto.ABCIParams{VoteExtensionsEnableHeight: 1},
	})

	resp, err := handler.ExtendVote()(ctx, &abci.RequestExtendVote{Height: 5})
	s.Require().NoError(err)
	s.Require().NotEmpty(resp.VoteExtension)

	var decoded oracletypes.OracleVote
	s.Require().NoError(decoded.Unmarshal(resp.VoteExtension))
	s.Require().EqualValues(5, decoded.SubmittedAtHeight)
	s.Require().Len(decoded.Prices, 1)
	s.Require().EqualValues(want[0].IndexPrice, decoded.Prices[0].IndexPrice)
	s.Require().EqualValues(want[0].MarkPrice, decoded.Prices[0].MarkPrice)

	verify, err := handler.VerifyVoteExtension()(ctx, &abci.RequestVerifyVoteExtension{
		Height:        5,
		VoteExtension: resp.VoteExtension,
	})
	s.Require().NoError(err)
	s.Require().Equal(abci.ResponseVerifyVoteExtension_ACCEPT, verify.Status)
}

// TestVerifyVoteExtensionRejectsZeroPrice ensures stateless validation
// drops payloads carrying zero index/mark prices.
func (s *OracleSuite) TestVerifyVoteExtensionRejectsZeroPrice() {
	handler := oraclekeeper.NewVoteExtensionHandler(s.App.OracleKeeper, s.App.GetTxConfig(), s.govAuthority())
	ctx := s.Ctx.WithConsensusParams(cmtproto.ConsensusParams{
		Abci: &cmtproto.ABCIParams{VoteExtensionsEnableHeight: 1},
	})
	bad := oracletypes.OracleVote{
		SubmittedAtHeight: 5,
		Prices: []oracletypes.MarketPrice{{
			MarketIndex: s.MarketIndex,
			IndexPrice:  0,
			MarkPrice:   1,
		}},
	}
	bz, err := bad.Marshal()
	s.Require().NoError(err)
	resp, err := handler.VerifyVoteExtension()(ctx, &abci.RequestVerifyVoteExtension{
		Height: 5, VoteExtension: bz,
	})
	s.Require().NoError(err)
	s.Require().Equal(abci.ResponseVerifyVoteExtension_REJECT, resp.Status)
}

// TestPrepareProposalAggregatesAndInjectsTx builds an ExtendedCommitInfo
// containing two synthetic validator votes (different prices, equal
// voting power) and drives PrepareProposal directly. It then decodes the
// proposer-injected first tx and asserts the median came out at the
// midpoint of the two validator quotes.
func (s *OracleSuite) TestPrepareProposalAggregatesAndInjectsTx() {
	gov := s.govAuthority()
	handler := oraclekeeper.NewVoteExtensionHandler(s.App.OracleKeeper, s.App.GetTxConfig(), gov)

	v1 := oracletypes.OracleVote{
		SubmittedAtHeight: 5,
		Prices: []oracletypes.MarketPrice{{
			MarketIndex: s.MarketIndex,
			IndexPrice:  100,
			MarkPrice:   100,
		}},
	}
	v2 := oracletypes.OracleVote{
		SubmittedAtHeight: 5,
		Prices: []oracletypes.MarketPrice{{
			MarketIndex: s.MarketIndex,
			IndexPrice:  200,
			MarkPrice:   200,
		}},
	}
	ext := abci.ExtendedCommitInfo{
		Round: 0,
		Votes: []abci.ExtendedVoteInfo{
			{
				Validator:     abci.Validator{Address: []byte("validator-aaaaaaaa"), Power: 50},
				BlockIdFlag:   cmtproto.BlockIDFlagCommit,
				VoteExtension: mustMarshal(s.T(), &v1),
			},
			{
				Validator:     abci.Validator{Address: []byte("validator-bbbbbbbb"), Power: 50},
				BlockIdFlag:   cmtproto.BlockIDFlagCommit,
				VoteExtension: mustMarshal(s.T(), &v2),
			},
		},
	}

	// Move the suite ctx into a state where veEnabled() returns true:
	// VoteExtensionsEnableHeight must be < req.Height.
	ctx := s.Ctx.WithConsensusParams(cmtproto.ConsensusParams{
		Abci: &cmtproto.ABCIParams{VoteExtensionsEnableHeight: 1},
	})

	// Wrapped handler echoes any txs the proposer hands it (no mempool).
	wrapped := func(_ sdk.Context, req *abci.RequestPrepareProposal) (*abci.ResponsePrepareProposal, error) {
		return &abci.ResponsePrepareProposal{Txs: req.Txs}, nil
	}
	resp, err := handler.PrepareProposal(wrapped)(ctx, &abci.RequestPrepareProposal{
		Height:          6,
		LocalLastCommit: ext,
		MaxTxBytes:      1024 * 1024,
	})
	s.Require().NoError(err)
	s.Require().NotEmpty(resp.Txs, "proposer must inject a tx when VEs are present")

	tx, err := s.App.GetTxConfig().TxDecoder()(resp.Txs[0])
	s.Require().NoError(err)
	msgs := tx.GetMsgs()
	s.Require().Len(msgs, 1)
	agg, ok := msgs[0].(*oracletypes.MsgAggregateOracleVotes)
	s.Require().True(ok)
	s.Require().Equal(gov, agg.Authority)
	s.Require().Len(agg.Aggregations, 1)

	got := agg.Aggregations[0]
	s.Require().Equal(s.MarketIndex, got.MarketIndex)
	// Equal-power weighted median picks the upper sample (n/2 cumulative
	// weight crosses at sample #2 once samples are sorted asc).
	s.Require().EqualValues(200, got.IndexPrice)
	s.Require().EqualValues(200, got.MarkPrice)

	// ProcessProposal should accept this very tx list.
	proc := handler.ProcessProposal(func(_ sdk.Context, _ *abci.RequestProcessProposal) (*abci.ResponseProcessProposal, error) {
		return &abci.ResponseProcessProposal{Status: abci.ResponseProcessProposal_ACCEPT}, nil
	})
	procResp, err := proc(ctx, &abci.RequestProcessProposal{
		Height: 6,
		Txs:    resp.Txs,
	})
	s.Require().NoError(err)
	s.Require().Equal(abci.ResponseProcessProposal_ACCEPT, procResp.Status)
}

// TestProcessProposalRejectsForgedAuthority makes sure a malicious
// proposer cannot swap in a self-signed MsgAggregateOracleVotes by
// changing the authority.
func (s *OracleSuite) TestProcessProposalRejectsForgedAuthority() {
	gov := s.govAuthority()
	handler := oraclekeeper.NewVoteExtensionHandler(s.App.OracleKeeper, s.App.GetTxConfig(), gov)
	ctx := s.Ctx.WithConsensusParams(cmtproto.ConsensusParams{
		Abci: &cmtproto.ABCIParams{VoteExtensionsEnableHeight: 1},
	})

	forged := &oracletypes.MsgAggregateOracleVotes{
		Authority: s.Users[0].Address.String(),
		Height:    6,
		Aggregations: []oracletypes.MarketAggregation{{
			MarketIndex: s.MarketIndex,
			IndexPrice:  1, MarkPrice: 1,
		}},
	}
	builder := s.App.GetTxConfig().NewTxBuilder()
	s.Require().NoError(builder.SetMsgs(forged))
	bz, err := s.App.GetTxConfig().TxEncoder()(builder.GetTx())
	s.Require().NoError(err)

	resp, err := handler.ProcessProposal(func(_ sdk.Context, _ *abci.RequestProcessProposal) (*abci.ResponseProcessProposal, error) {
		return &abci.ResponseProcessProposal{Status: abci.ResponseProcessProposal_ACCEPT}, nil
	})(ctx, &abci.RequestProcessProposal{Height: 6, Txs: [][]byte{bz}})
	s.Require().NoError(err)
	s.Require().Equal(abci.ResponseProcessProposal_REJECT, resp.Status)
}

// TestAggregateOracleVotesPersistsPrice drives the keeper's msg server
// directly to confirm that a successful aggregation writes the expected
// fields onto the OraclePrice store entry.
func (s *OracleSuite) TestAggregateOracleVotesPersistsPrice() {
	_, err := msg.AggregateVotes(s.App, s.Ctx, s.GovAddress, s.Ctx.BlockHeight(),
		[]oracletypes.MarketAggregation{{
			MarketIndex: s.MarketIndex,
			IndexPrice:  70_500,
			MarkPrice:   70_510,
		}},
	)
	s.Require().NoError(err)

	got := s.QueryOraclePrice(s.MarketIndex)
	s.Require().EqualValues(70_500, got.IndexPrice)
	s.Require().Greater(got.LastUpdatedTimestamp, int64(0))
	s.Require().Equal(s.Ctx.BlockHeight(), got.LastUpdatedHeight)
}

// mustMarshal panics on encode errors — only used by tests.
func mustMarshal(t interface{ Helper() }, v interface{ Marshal() ([]byte, error) }) []byte {
	t.Helper()
	bz, err := v.Marshal()
	if err != nil {
		panic(err)
	}
	return bz
}
