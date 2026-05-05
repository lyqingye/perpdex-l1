package keeper_test

import (
	"context"
	"testing"
	"time"

	abci "github.com/cometbft/cometbft/abci/types"
	cmtproto "github.com/cometbft/cometbft/proto/tendermint/types"
	"github.com/stretchr/testify/require"

	"cosmossdk.io/log"
	storetypes "cosmossdk.io/store/types"

	"github.com/cosmos/cosmos-sdk/runtime"
	"github.com/cosmos/cosmos-sdk/testutil/integration"
	sdk "github.com/cosmos/cosmos-sdk/types"
	moduletestutil "github.com/cosmos/cosmos-sdk/types/module/testutil"
	authtx "github.com/cosmos/cosmos-sdk/x/auth/tx"

	oracle "github.com/perpdex/perpdex-l1/x/oracle"
	oraclekeeper "github.com/perpdex/perpdex-l1/x/oracle/keeper"
	oracletypes "github.com/perpdex/perpdex-l1/x/oracle/types"
)

// govAddrFixture is the bech32 used for the gov authority across all
// vote-extension tests. We compute it on first access so the value is
// guaranteed to round-trip through `sdk.AccAddressFromBech32` regardless
// of the prefix the surrounding test environment has installed on
// `sdk.GetConfig`.
var govAddrFixture = func() string {
	return sdk.AccAddress(make([]byte, 20)).String()
}()

func newVEFixture(t *testing.T) (oraclekeeper.Keeper, sdk.Context, *oraclekeeper.VoteExtensionHandler) {
	t.Helper()
	keys := storetypes.NewKVStoreKeys(oracletypes.StoreKey)
	// Build an encoding config that knows about the oracle module so the
	// proposer-injected MsgAggregateOracleVotes can round-trip through
	// the tx codec. Without this the TxDecoder cannot resolve the
	// `/perpdex.oracle.v1.MsgAggregateOracleVotes` type URL.
	encCfg := moduletestutil.MakeTestEncodingConfig(oracle.AppModuleBasic{})
	cdc := encCfg.Codec
	cms := integration.CreateMultiStore(keys, log.NewTestLogger(t))
	ctx := sdk.NewContext(cms,
		cmtproto.Header{Time: time.Unix(1_700_000_000, 0)},
		false, log.NewTestLogger(t),
	).WithConsensusParams(cmtproto.ConsensusParams{
		Abci: &cmtproto.ABCIParams{VoteExtensionsEnableHeight: 1},
	})
	k := oraclekeeper.NewKeeper(cdc,
		runtime.NewKVStoreService(keys[oracletypes.StoreKey]),
		govAddrFixture,
	)
	require.NoError(t, k.Params.Set(ctx, oracletypes.DefaultParams()))
	txConfig := authtx.NewTxConfig(cdc, authtx.DefaultSignModes)
	h := oraclekeeper.NewVoteExtensionHandler(k, txConfig, govAddrFixture)
	return k, ctx, h
}

// TestExtendVote_UsesPriceFetcher confirms that whatever the price
// fetcher returns is what the validator emits as its vote extension.
func TestExtendVote_UsesPriceFetcher(t *testing.T) {
	k, ctx, h := newVEFixture(t)
	want := []oracletypes.MarketPrice{
		{MarketIndex: 1, IndexPrice: 100, MarkPrice: 101},
		{MarketIndex: 2, IndexPrice: 200, MarkPrice: 199},
	}
	k.SetPriceFetcher(oraclekeeper.PriceFetcherFunc(
		func(_ context.Context, _ int64) ([]oracletypes.MarketPrice, error) {
			return want, nil
		},
	))
	resp, err := h.ExtendVote()(ctx, &abci.RequestExtendVote{Height: 5})
	require.NoError(t, err)
	require.NotEmpty(t, resp.VoteExtension)

	var ov oracletypes.OracleVote
	require.NoError(t, ov.Unmarshal(resp.VoteExtension))
	require.EqualValues(t, 5, ov.SubmittedAtHeight)
	require.Len(t, ov.Prices, 2)
}

// TestExtendVote_FiltersZeroPrices drops zero-valued prices before
// emitting the vote extension. Zero prices would be rejected by the
// peer's VerifyVoteExtension anyway; pruning locally avoids producing
// invalid extensions.
func TestExtendVote_FiltersZeroPrices(t *testing.T) {
	k, ctx, h := newVEFixture(t)
	k.SetPriceFetcher(oraclekeeper.PriceFetcherFunc(
		func(_ context.Context, _ int64) ([]oracletypes.MarketPrice, error) {
			return []oracletypes.MarketPrice{
				{MarketIndex: 1, IndexPrice: 0, MarkPrice: 1},
				{MarketIndex: 2, IndexPrice: 1, MarkPrice: 0},
				{MarketIndex: 3, IndexPrice: 1, MarkPrice: 1},
			}, nil
		},
	))
	resp, err := h.ExtendVote()(ctx, &abci.RequestExtendVote{Height: 5})
	require.NoError(t, err)
	var ov oracletypes.OracleVote
	require.NoError(t, ov.Unmarshal(resp.VoteExtension))
	require.Len(t, ov.Prices, 1)
	require.EqualValues(t, 3, ov.Prices[0].MarketIndex)
}

// TestVerifyVoteExtension_RejectsHeightMismatch ensures the receiver
// drops payloads whose `submitted_at_height` does not match the
// containing prevote height.
func TestVerifyVoteExtension_RejectsHeightMismatch(t *testing.T) {
	_, ctx, h := newVEFixture(t)
	bz := mustMarshal(t, &oracletypes.OracleVote{
		SubmittedAtHeight: 99, // intentionally wrong
		Prices:            []oracletypes.MarketPrice{{MarketIndex: 1, IndexPrice: 1, MarkPrice: 1}},
	})
	resp, err := h.VerifyVoteExtension()(ctx, &abci.RequestVerifyVoteExtension{Height: 5, VoteExtension: bz})
	require.NoError(t, err)
	require.Equal(t, abci.ResponseVerifyVoteExtension_REJECT, resp.Status)
}

// TestVerifyVoteExtension_AcceptsEmpty allows validators to "abstain" on
// a single block by emitting an empty extension. This keeps the chain
// liveness intact when the local sidecar is briefly unavailable.
func TestVerifyVoteExtension_AcceptsEmpty(t *testing.T) {
	_, ctx, h := newVEFixture(t)
	resp, err := h.VerifyVoteExtension()(ctx, &abci.RequestVerifyVoteExtension{Height: 5, VoteExtension: nil})
	require.NoError(t, err)
	require.Equal(t, abci.ResponseVerifyVoteExtension_ACCEPT, resp.Status)
}

// TestPrepareProposal_WeightedMedian aggregates 3 validators with
// asymmetric voting power and asserts the proposer-injected
// MsgAggregateOracleVotes carries the weighted median.
func TestPrepareProposal_WeightedMedian(t *testing.T) {
	_, ctx, h := newVEFixture(t)
	v1 := mustMarshal(t, &oracletypes.OracleVote{
		SubmittedAtHeight: 5,
		Prices:            []oracletypes.MarketPrice{{MarketIndex: 1, IndexPrice: 100, MarkPrice: 100}},
	})
	v2 := mustMarshal(t, &oracletypes.OracleVote{
		SubmittedAtHeight: 5,
		Prices:            []oracletypes.MarketPrice{{MarketIndex: 1, IndexPrice: 200, MarkPrice: 200}},
	})
	v3 := mustMarshal(t, &oracletypes.OracleVote{
		SubmittedAtHeight: 5,
		Prices:            []oracletypes.MarketPrice{{MarketIndex: 1, IndexPrice: 150, MarkPrice: 150}},
	})
	ext := abci.ExtendedCommitInfo{
		Round: 0,
		Votes: []abci.ExtendedVoteInfo{
			{Validator: abci.Validator{Address: []byte("a"), Power: 50}, BlockIdFlag: cmtproto.BlockIDFlagCommit, VoteExtension: v1},
			{Validator: abci.Validator{Address: []byte("b"), Power: 30}, BlockIdFlag: cmtproto.BlockIDFlagCommit, VoteExtension: v2},
			{Validator: abci.Validator{Address: []byte("c"), Power: 20}, BlockIdFlag: cmtproto.BlockIDFlagCommit, VoteExtension: v3},
		},
	}

	wrapped := func(_ sdk.Context, req *abci.RequestPrepareProposal) (*abci.ResponsePrepareProposal, error) {
		return &abci.ResponsePrepareProposal{Txs: req.Txs}, nil
	}
	resp, err := h.PrepareProposal(wrapped)(ctx, &abci.RequestPrepareProposal{
		Height:          6,
		LocalLastCommit: ext,
		MaxTxBytes:      1024 * 1024,
	})
	require.NoError(t, err)
	require.NotEmpty(t, resp.Txs)

	tx, err := veTestTxConfig(t).TxDecoder()(resp.Txs[0])
	require.NoError(t, err)
	msgs := tx.GetMsgs()
	require.Len(t, msgs, 1)
	agg, ok := msgs[0].(*oracletypes.MsgAggregateOracleVotes)
	require.True(t, ok)
	require.Len(t, agg.Aggregations, 1)
	got := agg.Aggregations[0]
	// Sorted samples: 100(50), 150(20), 200(30); cumulative reaches 50%
	// (50 of 100) at sample #1 (=100). With strict ">= half AND > half"
	// crossing semantics in our WeightedMedian, the median equals 150.
	require.EqualValues(t, 150, got.IndexPrice)
	require.EqualValues(t, 150, got.MarkPrice)
}

// veTestTxConfig returns the same encoding-config-aware TxConfig the
// fixture installs into the handler so encode/decode round-trip in tests.
func veTestTxConfig(t *testing.T) sdkClientTxConfig {
	t.Helper()
	encCfg := moduletestutil.MakeTestEncodingConfig(oracle.AppModuleBasic{})
	return authtx.NewTxConfig(encCfg.Codec, authtx.DefaultSignModes)
}

// sdkClientTxConfig is a local alias to keep the helper signature
// readable without re-importing the sdk client package in every test.
type sdkClientTxConfig = interface {
	TxDecoder() sdk.TxDecoder
	TxEncoder() sdk.TxEncoder
}

// TestPrepareProposal_QuorumNotReached drops markets whose participating
// voting power is below `Params.MinVotingPowerRatio`.
func TestPrepareProposal_QuorumNotReached(t *testing.T) {
	k, ctx, h := newVEFixture(t)
	// Tighten the quorum to 90% so a single 10% voter cannot pass.
	params, err := k.Params.Get(ctx)
	require.NoError(t, err)
	params.MinVotingPowerRatio = 9_000
	require.NoError(t, k.Params.Set(ctx, params))

	v := mustMarshal(t, &oracletypes.OracleVote{
		SubmittedAtHeight: 5,
		Prices:            []oracletypes.MarketPrice{{MarketIndex: 1, IndexPrice: 100, MarkPrice: 100}},
	})
	ext := abci.ExtendedCommitInfo{
		Votes: []abci.ExtendedVoteInfo{
			{Validator: abci.Validator{Address: []byte("a"), Power: 10}, BlockIdFlag: cmtproto.BlockIDFlagCommit, VoteExtension: v},
			{Validator: abci.Validator{Address: []byte("b"), Power: 90}, BlockIdFlag: cmtproto.BlockIDFlagCommit, VoteExtension: nil},
		},
	}
	wrapped := func(_ sdk.Context, _ *abci.RequestPrepareProposal) (*abci.ResponsePrepareProposal, error) {
		return &abci.ResponsePrepareProposal{Txs: nil}, nil
	}
	resp, err := h.PrepareProposal(wrapped)(ctx, &abci.RequestPrepareProposal{
		Height:          6,
		LocalLastCommit: ext,
		MaxTxBytes:      1024 * 1024,
	})
	require.NoError(t, err)
	require.Empty(t, resp.Txs, "no quorum -> proposer must not inject a stale aggregation")
}

// TestProcessProposal_RejectsMissingTx fails the proposal when the
// proposer forgot to inject MsgAggregateOracleVotes.
func TestProcessProposal_RejectsMissingTx(t *testing.T) {
	_, ctx, h := newVEFixture(t)
	wrapped := func(_ sdk.Context, _ *abci.RequestProcessProposal) (*abci.ResponseProcessProposal, error) {
		t.Fatalf("wrapped handler should not be reached when oracle tx is missing")
		return nil, nil
	}
	resp, err := h.ProcessProposal(wrapped)(ctx, &abci.RequestProcessProposal{Height: 6, Txs: nil})
	require.NoError(t, err)
	require.Equal(t, abci.ResponseProcessProposal_REJECT, resp.Status)
}

// TestProcessProposal_AcceptsWellFormed walks the entire prepare ->
// process round-trip and confirms a wrapped handler call happens iff
// validation succeeds.
func TestProcessProposal_AcceptsWellFormed(t *testing.T) {
	_, ctx, h := newVEFixture(t)
	v := mustMarshal(t, &oracletypes.OracleVote{
		SubmittedAtHeight: 5,
		Prices:            []oracletypes.MarketPrice{{MarketIndex: 1, IndexPrice: 100, MarkPrice: 100}},
	})
	ext := abci.ExtendedCommitInfo{
		Votes: []abci.ExtendedVoteInfo{
			{Validator: abci.Validator{Address: []byte("a"), Power: 100}, BlockIdFlag: cmtproto.BlockIDFlagCommit, VoteExtension: v},
		},
	}
	prepResp, err := h.PrepareProposal(func(_ sdk.Context, req *abci.RequestPrepareProposal) (*abci.ResponsePrepareProposal, error) {
		return &abci.ResponsePrepareProposal{Txs: req.Txs}, nil
	})(ctx, &abci.RequestPrepareProposal{Height: 6, LocalLastCommit: ext, MaxTxBytes: 1024 * 1024})
	require.NoError(t, err)
	require.NotEmpty(t, prepResp.Txs)

	called := false
	procResp, err := h.ProcessProposal(func(_ sdk.Context, _ *abci.RequestProcessProposal) (*abci.ResponseProcessProposal, error) {
		called = true
		return &abci.ResponseProcessProposal{Status: abci.ResponseProcessProposal_ACCEPT}, nil
	})(ctx, &abci.RequestProcessProposal{Height: 6, Txs: prepResp.Txs})
	require.NoError(t, err)
	require.True(t, called, "ProcessProposal should defer to the wrapped handler after validation passes")
	require.Equal(t, abci.ResponseProcessProposal_ACCEPT, procResp.Status)
}

// mustMarshal panics on encode errors — only used by tests.
func mustMarshal(t *testing.T, v interface{ Marshal() ([]byte, error) }) []byte {
	t.Helper()
	bz, err := v.Marshal()
	require.NoError(t, err)
	return bz
}
