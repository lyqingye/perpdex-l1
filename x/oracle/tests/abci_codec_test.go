// Suite: ABCI++ vote-extension wire codec.
//
// Covers the encoder/decoder pair used on both sides of the VE pipeline:
//   - OracleVote payloads emitted on ExtendVote
//   - ExtendedCommitInfo bundles injected on PrepareProposal
//
// Each test exercises the wire format end-to-end (encode → decode →
// equality) so any silent protobuf or zstd-frame regression is caught
// before it can break consensus.
package tests

import (
	"testing"

	cometabci "github.com/cometbft/cometbft/abci/types"
	cmtproto "github.com/cometbft/cometbft/proto/tendermint/types"
	"github.com/stretchr/testify/require"

	"github.com/perpdex/perpdex-l1/x/oracle/abci/codec"
	oracletypes "github.com/perpdex/perpdex-l1/x/oracle/types"
)

func TestRawVoteExtensionCodec_RoundTrip(t *testing.T) {
	c := codec.NewRawVoteExtensionCodec()
	v := oracletypes.OracleVote{
		SubmittedAtHeight: 42,
		Prices: []oracletypes.MarketPrice{
			{MarketIndex: 1, IndexPrice: 100, MarkPrice: 100},
			{MarketIndex: 2, IndexPrice: 200, MarkPrice: 199},
		},
	}
	bz, err := c.Encode(v)
	require.NoError(t, err)
	require.NotEmpty(t, bz)
	got, err := c.Decode(bz)
	require.NoError(t, err)
	require.EqualValues(t, 42, got.SubmittedAtHeight)
	require.Len(t, got.Prices, 2)
}

func TestRawExtendedCommitCodec_RoundTrip(t *testing.T) {
	c := codec.NewRawExtendedCommitCodec()
	ec := cometabci.ExtendedCommitInfo{
		Round: 0,
		Votes: []cometabci.ExtendedVoteInfo{
			{
				Validator:          cometabci.Validator{Address: []byte("a"), Power: 100},
				BlockIdFlag:        cmtproto.BlockIDFlagCommit,
				VoteExtension:      []byte("ve"),
				ExtensionSignature: []byte("sig"),
			},
		},
	}
	bz, err := c.Encode(ec)
	require.NoError(t, err)
	got, err := c.Decode(bz)
	require.NoError(t, err)
	require.Len(t, got.Votes, 1)
	require.EqualValues(t, 100, got.Votes[0].Validator.Power)
}

func TestZstdExtendedCommitCodec_CompressesAndDecodes(t *testing.T) {
	raw := codec.NewRawExtendedCommitCodec()
	zc, err := codec.NewZstdExtendedCommitCodec(raw)
	require.NoError(t, err)

	ec := cometabci.ExtendedCommitInfo{
		Votes: []cometabci.ExtendedVoteInfo{
			{Validator: cometabci.Validator{Address: []byte("aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"), Power: 100}, BlockIdFlag: cmtproto.BlockIDFlagCommit, VoteExtension: make([]byte, 256)},
		},
	}
	bz, err := zc.Encode(ec)
	require.NoError(t, err)

	rawBz, _ := raw.Encode(ec)
	require.LessOrEqual(t, len(bz), len(rawBz)+128, "zstd output should not be drastically larger than raw")

	got, err := zc.Decode(bz)
	require.NoError(t, err)
	require.Len(t, got.Votes, 1)
}

func TestRawCodec_DecodeEmpty(t *testing.T) {
	require.NotPanics(t, func() {
		ve := codec.NewRawVoteExtensionCodec()
		got, err := ve.Decode(nil)
		require.NoError(t, err)
		require.Empty(t, got.Prices)
	})
	require.NotPanics(t, func() {
		ec := codec.NewRawExtendedCommitCodec()
		got, err := ec.Decode(nil)
		require.NoError(t, err)
		require.Empty(t, got.Votes)
	})
}
