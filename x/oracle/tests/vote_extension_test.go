// Suite: ABCI++ vote-extension pipeline (ExtendVote → VerifyVoteExtension
// → PrepareProposal → ProcessProposal → PreBlocker).
//
// Drives the keeper's `VoteExtensionHandler` end-to-end with the raw
// codecs to assert:
//   - `ExtendVote` consults whatever PriceFetcher is wired and prunes
//     zero-valued prices before publishing the VE.
//   - `VerifyVoteExtension` rejects payloads with a height mismatch and
//     accepts empty (abstaining) VEs so liveness is preserved when a
//     local sidecar is briefly unavailable.
//   - The proposer pipeline injects `ExtendedCommitInfo` verbatim as
//     `Txs[0]` and the receiver enforces the 2/3+ supermajority rule.
//   - `PreBlocker` writes the per-market weighted median to the
//     `OraclePrice` store with EMA smoothing disabled for determinism.
package tests

import (
	"context"
	"testing"
	"time"

	abci "github.com/cometbft/cometbft/abci/types"
	cmtproto "github.com/cometbft/cometbft/proto/tendermint/types"
	"github.com/stretchr/testify/require"

	sdk "github.com/cosmos/cosmos-sdk/types"

	oraclekeeper "github.com/perpdex/perpdex-l1/x/oracle/keeper"
	oracletypes "github.com/perpdex/perpdex-l1/x/oracle/types"
)

// TestExtendVote_UsesPriceFetcher confirms that whatever the price
// fetcher returns is what the validator emits as its vote extension.
func TestExtendVote_UsesPriceFetcher(t *testing.T) {
	k, ctx, h, veCodec, _ := newVEFixture(t)
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

	ov, err := veCodec.Decode(resp.VoteExtension)
	require.NoError(t, err)
	require.EqualValues(t, 5, ov.SubmittedAtHeight)
	require.Len(t, ov.Prices, 2)
}

// TestExtendVote_FiltersZeroPrices drops zero-valued prices before
// emitting the vote extension. Zero prices would be rejected by the
// peer's VerifyVoteExtension anyway; pruning locally avoids producing
// invalid extensions.
func TestExtendVote_FiltersZeroPrices(t *testing.T) {
	k, ctx, h, veCodec, _ := newVEFixture(t)
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
	ov, err := veCodec.Decode(resp.VoteExtension)
	require.NoError(t, err)
	require.Len(t, ov.Prices, 1)
	require.EqualValues(t, 3, ov.Prices[0].MarketIndex)
}

// TestVerifyVoteExtension_RejectsHeightMismatch ensures the receiver
// drops payloads whose `submitted_at_height` does not match the
// containing prevote height.
func TestVerifyVoteExtension_RejectsHeightMismatch(t *testing.T) {
	_, ctx, h, veCodec, _ := newVEFixture(t)
	bz, err := veCodec.Encode(oracletypes.OracleVote{
		SubmittedAtHeight: 99,
		Prices:            []oracletypes.MarketPrice{{MarketIndex: 1, IndexPrice: 1, MarkPrice: 1}},
	})
	require.NoError(t, err)
	resp, err := h.VerifyVoteExtension()(ctx, &abci.RequestVerifyVoteExtension{Height: 5, VoteExtension: bz})
	require.NoError(t, err)
	require.Equal(t, abci.ResponseVerifyVoteExtension_REJECT, resp.Status)
}

// TestVerifyVoteExtension_AcceptsEmpty allows validators to "abstain" on
// a single block by emitting an empty extension. This keeps the chain
// liveness intact when the local sidecar is briefly unavailable.
func TestVerifyVoteExtension_AcceptsEmpty(t *testing.T) {
	_, ctx, h, _, _ := newVEFixture(t)
	resp, err := h.VerifyVoteExtension()(ctx, &abci.RequestVerifyVoteExtension{Height: 5, VoteExtension: nil})
	require.NoError(t, err)
	require.Equal(t, abci.ResponseVerifyVoteExtension_ACCEPT, resp.Status)
}

// TestPrepareProposal_InjectsExtendedCommit verifies the proposer
// injects the previous block's ExtendedCommitInfo as Txs[0] verbatim.
// Aggregation happens later in PreBlock; PrepareProposal should NOT
// produce SDK transactions.
func TestPrepareProposal_InjectsExtendedCommit(t *testing.T) {
	_, ctx, h, veCodec, ecCodec := newVEFixture(t)
	v1, _ := veCodec.Encode(oracletypes.OracleVote{
		SubmittedAtHeight: 5,
		Prices:            []oracletypes.MarketPrice{{MarketIndex: 1, IndexPrice: 100, MarkPrice: 100}},
	})
	ext := abci.ExtendedCommitInfo{
		Round: 0,
		Votes: []abci.ExtendedVoteInfo{
			{Validator: abci.Validator{Address: []byte("a"), Power: 100}, BlockIdFlag: cmtproto.BlockIDFlagCommit, VoteExtension: v1},
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

	// Txs[0] must round-trip back through the EC codec.
	decoded, err := ecCodec.Decode(resp.Txs[0])
	require.NoError(t, err)
	require.Len(t, decoded.Votes, 1)
	require.EqualValues(t, 100, decoded.Votes[0].Validator.Power)
}

// TestProcessProposal_RejectsMissingTx fails the proposal when the
// proposer forgot to inject ExtendedCommitInfo.
func TestProcessProposal_RejectsMissingTx(t *testing.T) {
	_, ctx, h, _, _ := newVEFixture(t)
	wrapped := func(_ sdk.Context, _ *abci.RequestProcessProposal) (*abci.ResponseProcessProposal, error) {
		t.Fatalf("wrapped handler should not be reached when ext info is missing")
		return nil, nil
	}
	resp, err := h.ProcessProposal(wrapped)(ctx, &abci.RequestProcessProposal{Height: 6, Txs: nil})
	require.NoError(t, err)
	require.Equal(t, abci.ResponseProcessProposal_REJECT, resp.Status)
}

// TestProcessProposal_RejectsMinorityPower asserts that ProcessProposal
// rejects a block whose ExtendedCommitInfo only has a 1/3 commit (below
// the 2/3+ supermajority threshold).
func TestProcessProposal_RejectsMinorityPower(t *testing.T) {
	_, ctx, h, _, ecCodec := newVEFixture(t)
	ext := abci.ExtendedCommitInfo{
		Votes: []abci.ExtendedVoteInfo{
			{Validator: abci.Validator{Address: []byte("a"), Power: 30}, BlockIdFlag: cmtproto.BlockIDFlagCommit},
			{Validator: abci.Validator{Address: []byte("b"), Power: 70}, BlockIdFlag: cmtproto.BlockIDFlagAbsent},
		},
	}
	bz, err := ecCodec.Encode(ext)
	require.NoError(t, err)
	wrapped := func(_ sdk.Context, _ *abci.RequestProcessProposal) (*abci.ResponseProcessProposal, error) {
		t.Fatalf("wrapped handler should not be reached on minority commit")
		return nil, nil
	}
	resp, err := h.ProcessProposal(wrapped)(ctx, &abci.RequestProcessProposal{
		Height: 6,
		Txs:    [][]byte{bz},
	})
	require.NoError(t, err)
	require.Equal(t, abci.ResponseProcessProposal_REJECT, resp.Status)
}

// TestProcessProposal_AcceptsSupermajority drives the full prepare ->
// process pipeline and confirms the wrapped handler is reached when the
// commit info has a 2/3+ supermajority.
func TestProcessProposal_AcceptsSupermajority(t *testing.T) {
	_, ctx, h, veCodec, _ := newVEFixture(t)
	v, err := veCodec.Encode(oracletypes.OracleVote{
		SubmittedAtHeight: 5,
		Prices:            []oracletypes.MarketPrice{{MarketIndex: 1, IndexPrice: 100, MarkPrice: 100}},
	})
	require.NoError(t, err)
	ext := abci.ExtendedCommitInfo{
		Votes: []abci.ExtendedVoteInfo{
			{Validator: abci.Validator{Address: []byte("a"), Power: 100}, BlockIdFlag: cmtproto.BlockIDFlagCommit, VoteExtension: v, ExtensionSignature: []byte("sig")},
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

// TestPreBlocker_WeightedMedianPersisted exercises the full PreBlocker
// path: starts with an ExtendedCommitInfo with three asymmetric voters,
// drives ProcessProposal (validation), and asserts the per-market
// weighted median is written to OraclePrice store.
func TestPreBlocker_WeightedMedianPersisted(t *testing.T) {
	k, ctx, h, veCodec, ecCodec := newVEFixture(t)

	v1, _ := veCodec.Encode(oracletypes.OracleVote{
		SubmittedAtHeight: 5,
		Prices:            []oracletypes.MarketPrice{{MarketIndex: 1, IndexPrice: 100, MarkPrice: 100}},
	})
	v2, _ := veCodec.Encode(oracletypes.OracleVote{
		SubmittedAtHeight: 5,
		Prices:            []oracletypes.MarketPrice{{MarketIndex: 1, IndexPrice: 200, MarkPrice: 200}},
	})
	v3, _ := veCodec.Encode(oracletypes.OracleVote{
		SubmittedAtHeight: 5,
		Prices:            []oracletypes.MarketPrice{{MarketIndex: 1, IndexPrice: 150, MarkPrice: 150}},
	})
	ext := abci.ExtendedCommitInfo{
		Votes: []abci.ExtendedVoteInfo{
			{Validator: abci.Validator{Address: []byte("a"), Power: 50}, BlockIdFlag: cmtproto.BlockIDFlagCommit, VoteExtension: v1, ExtensionSignature: []byte("s1")},
			{Validator: abci.Validator{Address: []byte("b"), Power: 30}, BlockIdFlag: cmtproto.BlockIDFlagCommit, VoteExtension: v2, ExtensionSignature: []byte("s2")},
			{Validator: abci.Validator{Address: []byte("c"), Power: 20}, BlockIdFlag: cmtproto.BlockIDFlagCommit, VoteExtension: v3, ExtensionSignature: []byte("s3")},
		},
	}
	bz, err := ecCodec.Encode(ext)
	require.NoError(t, err)

	// Disable EMA smoothing for a deterministic median check.
	params, err := k.Params.Get(ctx)
	require.NoError(t, err)
	params.MarkPriceEmaAlpha = 0
	require.NoError(t, k.Params.Set(ctx, params))

	preResp, err := h.PreBlocker()(ctx, &abci.RequestFinalizeBlock{
		Height: 6,
		Txs:    [][]byte{bz},
		Time:   time.Unix(1_700_000_001, 0),
	})
	require.NoError(t, err)
	require.NotNil(t, preResp)

	got, err := k.GetPrice(ctx, 1)
	require.NoError(t, err)
	// Sorted samples: 100(50), 150(20), 200(30); cumulative reaches 50%
	// (50 of 100) at sample #1 (=100). With ">= half AND > half" the
	// median is the next bucket = 150.
	require.EqualValues(t, 150, got.IndexPrice)
	require.EqualValues(t, 150, got.MarkPrice)
}
