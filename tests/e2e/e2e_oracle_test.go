package e2e_test

import (
	"context"
	"testing"
	"time"

	abci "github.com/cometbft/cometbft/abci/types"
	cmtproto "github.com/cometbft/cometbft/proto/tendermint/types"

	sdk "github.com/cosmos/cosmos-sdk/types"

	perptypes "github.com/perpdex/perpdex-l1/types"
	oraclecodec "github.com/perpdex/perpdex-l1/x/oracle/abci/codec"
	oraclekeeper "github.com/perpdex/perpdex-l1/x/oracle/keeper"
	oracletypes "github.com/perpdex/perpdex-l1/x/oracle/types"

	"github.com/perpdex/perpdex-l1/tests/e2e"
	"github.com/perpdex/perpdex-l1/tests/e2e/msg"
)

// OracleSuite covers the dydx/Connect-style ABCI++ vote-extension oracle
// pipeline end-to-end:
//
//  1. Genesis params default to vote-extension-enabled, max_age_ms > 0.
//  2. ExtendVote → VerifyVoteExtension stateless contract: PriceFetcher
//     output round-trips through the codec and Verify accepts well-formed
//     payloads / rejects zero-priced ones.
//  3. PrepareProposal injects the previous block's ExtendedCommitInfo as
//     Txs[0] verbatim; ProcessProposal re-validates the supermajority.
//  4. PreBlocker decodes Txs[0], runs the weighted median per market and
//     persists the aggregated price into state.
//  5. ProcessProposal rejects a proposal whose Txs[0] only carries minority
//     voting power.
type OracleSuite struct {
	e2e.PerpdexTestSuite

	BTCAssetIndex uint32
	MarketIndex   uint32

	veCodec oraclecodec.VoteExtensionCodec
	ecCodec oraclecodec.ExtendedCommitCodec
	handler *oraclekeeper.VoteExtensionHandler
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

	s.veCodec = oraclecodec.NewRawVoteExtensionCodec()
	s.ecCodec = oraclecodec.NewRawExtendedCommitCodec()
	s.handler = oraclekeeper.NewVoteExtensionHandler(s.App.OracleKeeper, s.veCodec, s.ecCodec)
}

func (s *OracleSuite) veCtx() sdk.Context {
	return s.Ctx.WithConsensusParams(cmtproto.ConsensusParams{
		Abci: &cmtproto.ABCIParams{VoteExtensionsEnableHeight: 1},
	})
}

// TestParamsDefaults confirms the genesis-supplied params keep the
// pipeline active without any governance interaction.
func (s *OracleSuite) TestParamsDefaults() {
	params := s.QueryOracleParams()
	s.Require().True(params.VoteExtensionEnabled)
	s.Require().Greater(params.MaxAgeMs, int64(0))
}

// TestExtendVoteRoundTrip exercises ExtendVote + VerifyVoteExtension as a
// single contract by injecting a deterministic PriceFetcher.
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
	ctx := s.veCtx()

	resp, err := s.handler.ExtendVote()(ctx, &abci.RequestExtendVote{Height: 5})
	s.Require().NoError(err)
	s.Require().NotEmpty(resp.VoteExtension)

	decoded, err := s.veCodec.Decode(resp.VoteExtension)
	s.Require().NoError(err)
	s.Require().EqualValues(5, decoded.SubmittedAtHeight)
	s.Require().Len(decoded.Prices, 1)
	s.Require().EqualValues(want[0].IndexPrice, decoded.Prices[0].IndexPrice)
	s.Require().EqualValues(want[0].MarkPrice, decoded.Prices[0].MarkPrice)

	verify, err := s.handler.VerifyVoteExtension()(ctx, &abci.RequestVerifyVoteExtension{
		Height:        5,
		VoteExtension: resp.VoteExtension,
	})
	s.Require().NoError(err)
	s.Require().Equal(abci.ResponseVerifyVoteExtension_ACCEPT, verify.Status)
}

// TestVerifyVoteExtensionRejectsZeroPrice ensures stateless validation
// drops payloads carrying zero index/mark prices.
func (s *OracleSuite) TestVerifyVoteExtensionRejectsZeroPrice() {
	ctx := s.veCtx()
	bad := oracletypes.OracleVote{
		SubmittedAtHeight: 5,
		Prices: []oracletypes.MarketPrice{{
			MarketIndex: s.MarketIndex,
			IndexPrice:  0,
			MarkPrice:   1,
		}},
	}
	bz, err := s.veCodec.Encode(bad)
	s.Require().NoError(err)
	resp, err := s.handler.VerifyVoteExtension()(ctx, &abci.RequestVerifyVoteExtension{
		Height: 5, VoteExtension: bz,
	})
	s.Require().NoError(err)
	s.Require().Equal(abci.ResponseVerifyVoteExtension_REJECT, resp.Status)
}

// TestPipelineInjectsExtendedCommitAndPersists drives ExtendedCommitInfo
// through PrepareProposal (injection) → ProcessProposal (validation) →
// PreBlocker (aggregation + state write).
func (s *OracleSuite) TestPipelineInjectsExtendedCommitAndPersists() {
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
	b1, _ := s.veCodec.Encode(v1)
	b2, _ := s.veCodec.Encode(v2)
	ext := abci.ExtendedCommitInfo{
		Round: 0,
		Votes: []abci.ExtendedVoteInfo{
			{Validator: abci.Validator{Address: []byte("validator-aaaaaaaa"), Power: 50}, BlockIdFlag: cmtproto.BlockIDFlagCommit, VoteExtension: b1, ExtensionSignature: []byte("s1")},
			{Validator: abci.Validator{Address: []byte("validator-bbbbbbbb"), Power: 50}, BlockIdFlag: cmtproto.BlockIDFlagCommit, VoteExtension: b2, ExtensionSignature: []byte("s2")},
		},
	}

	ctx := s.veCtx()
	wrapped := func(_ sdk.Context, req *abci.RequestPrepareProposal) (*abci.ResponsePrepareProposal, error) {
		return &abci.ResponsePrepareProposal{Txs: req.Txs}, nil
	}
	prep, err := s.handler.PrepareProposal(wrapped)(ctx, &abci.RequestPrepareProposal{
		Height:          6,
		LocalLastCommit: ext,
		MaxTxBytes:      1024 * 1024,
	})
	s.Require().NoError(err)
	s.Require().NotEmpty(prep.Txs, "proposer must inject Txs[0]")

	// Round-trip the injected bytes through the codec.
	decodedExt, err := s.ecCodec.Decode(prep.Txs[0])
	s.Require().NoError(err)
	s.Require().Len(decodedExt.Votes, 2)

	// ProcessProposal should accept (2/2 commit power = 100%).
	procResp, err := s.handler.ProcessProposal(func(_ sdk.Context, _ *abci.RequestProcessProposal) (*abci.ResponseProcessProposal, error) {
		return &abci.ResponseProcessProposal{Status: abci.ResponseProcessProposal_ACCEPT}, nil
	})(ctx, &abci.RequestProcessProposal{Height: 6, Txs: prep.Txs})
	s.Require().NoError(err)
	s.Require().Equal(abci.ResponseProcessProposal_ACCEPT, procResp.Status)

	// PreBlocker should aggregate and persist.
	params, err := s.App.OracleKeeper.Params.Get(ctx)
	s.Require().NoError(err)
	params.MarkPriceEmaAlpha = 0
	s.Require().NoError(s.App.OracleKeeper.Params.Set(ctx, params))

	preCtx := ctx.WithBlockHeight(6).WithBlockTime(time.Unix(1_700_000_500, 0))
	_, err = s.handler.PreBlocker()(preCtx, &abci.RequestFinalizeBlock{
		Height: 6,
		Txs:    prep.Txs,
		Time:   time.Unix(1_700_000_500, 0),
	})
	s.Require().NoError(err)

	got, err := s.App.OracleKeeper.GetPrice(preCtx, s.MarketIndex)
	s.Require().NoError(err)
	// Equal-weight median picks the upper sample (n/2 cumulative weight).
	s.Require().EqualValues(200, got.IndexPrice)
	s.Require().EqualValues(200, got.MarkPrice)
	s.Require().Equal(int64(6), got.LastUpdatedHeight)
}

// TestProcessProposalRejectsMinority asserts that a proposal whose Txs[0]
// only carries 1/3 of voting power is rejected.
func (s *OracleSuite) TestProcessProposalRejectsMinority() {
	ext := abci.ExtendedCommitInfo{
		Votes: []abci.ExtendedVoteInfo{
			{Validator: abci.Validator{Address: []byte("a"), Power: 30}, BlockIdFlag: cmtproto.BlockIDFlagCommit},
			{Validator: abci.Validator{Address: []byte("b"), Power: 70}, BlockIdFlag: cmtproto.BlockIDFlagAbsent},
		},
	}
	bz, err := s.ecCodec.Encode(ext)
	s.Require().NoError(err)

	resp, err := s.handler.ProcessProposal(func(_ sdk.Context, _ *abci.RequestProcessProposal) (*abci.ResponseProcessProposal, error) {
		s.T().Fatalf("wrapped handler must not be called on minority proposal")
		return nil, nil
	})(s.veCtx(), &abci.RequestProcessProposal{Height: 6, Txs: [][]byte{bz}})
	s.Require().NoError(err)
	s.Require().Equal(abci.ResponseProcessProposal_REJECT, resp.Status)
}
