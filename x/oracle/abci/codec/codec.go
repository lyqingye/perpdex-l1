// Package codec defines the wire encoders used by the oracle ABCI++ vote-
// extension pipeline.
//
// Two distinct shapes are encoded:
//
//   1. VoteExtension: emitted by every validator on ExtendVote and consumed
//      by the proposer on PrepareProposal. Concretely a marshalled
//      `oracletypes.OracleVote`.
//
//   2. ExtendedCommitInfo: produced by the proposer on PrepareProposal and
//      consumed by every validator on ProcessProposal + the chain itself
//      on PreBlock. It is a `cometabci.ExtendedCommitInfo` value (from
//      cometbft) that wraps every committed validator's vote extension
//      from the previous block.
//
// Both encoders ship in two flavours:
//
//   - RawCodec is a thin wrapper over the proto Marshal/Unmarshal calls.
//     It is the simplest correct choice and is what unit tests use.
//   - ZstdCodec is the production choice; it nests RawCodec inside a zstd
//     frame so multi-validator commit info (which can grow to several KB
//     across 100 validators × 32 markets) does not blow past the cometbft
//     1MB block size cap. The frame format is `<zstd>(raw)`; we leave the
//     framing implicit because the ZstdCodec is the sole producer.
//
// Operators wire the codec they want at app construction time; downgrading
// from Zstd to Raw across an upgrade is a state-incompatible change because
// previously committed blocks reference the Zstd frame.
package codec

import (
	"fmt"

	cometabci "github.com/cometbft/cometbft/abci/types"
	"github.com/klauspost/compress/zstd"

	oracletypes "github.com/perpdex/perpdex-l1/x/oracle/types"
)

// VoteExtensionCodec encodes/decodes the per-validator OracleVote payload.
type VoteExtensionCodec interface {
	Encode(oracletypes.OracleVote) ([]byte, error)
	Decode([]byte) (oracletypes.OracleVote, error)
}

// ExtendedCommitCodec encodes/decodes the ExtendedCommitInfo bundle.
type ExtendedCommitCodec interface {
	Encode(cometabci.ExtendedCommitInfo) ([]byte, error)
	Decode([]byte) (cometabci.ExtendedCommitInfo, error)
}

// RawVoteExtensionCodec is the no-compression VoteExtensionCodec.
type RawVoteExtensionCodec struct{}

// NewRawVoteExtensionCodec returns the raw VE codec.
func NewRawVoteExtensionCodec() RawVoteExtensionCodec { return RawVoteExtensionCodec{} }

func (RawVoteExtensionCodec) Encode(v oracletypes.OracleVote) ([]byte, error) {
	bz, err := v.Marshal()
	if err != nil {
		return nil, fmt.Errorf("oracle codec: marshal OracleVote: %w", err)
	}
	return bz, nil
}

func (RawVoteExtensionCodec) Decode(bz []byte) (oracletypes.OracleVote, error) {
	if len(bz) == 0 {
		return oracletypes.OracleVote{}, nil
	}
	var v oracletypes.OracleVote
	if err := v.Unmarshal(bz); err != nil {
		return oracletypes.OracleVote{}, fmt.Errorf("oracle codec: unmarshal OracleVote: %w", err)
	}
	return v, nil
}

// RawExtendedCommitCodec is the no-compression ExtendedCommitCodec.
type RawExtendedCommitCodec struct{}

// NewRawExtendedCommitCodec returns the raw EC codec.
func NewRawExtendedCommitCodec() RawExtendedCommitCodec { return RawExtendedCommitCodec{} }

func (RawExtendedCommitCodec) Encode(ec cometabci.ExtendedCommitInfo) ([]byte, error) {
	bz, err := ec.Marshal()
	if err != nil {
		return nil, fmt.Errorf("oracle codec: marshal ExtendedCommitInfo: %w", err)
	}
	return bz, nil
}

func (RawExtendedCommitCodec) Decode(bz []byte) (cometabci.ExtendedCommitInfo, error) {
	if len(bz) == 0 {
		return cometabci.ExtendedCommitInfo{}, nil
	}
	var ec cometabci.ExtendedCommitInfo
	if err := ec.Unmarshal(bz); err != nil {
		return cometabci.ExtendedCommitInfo{}, fmt.Errorf("oracle codec: unmarshal ExtendedCommitInfo: %w", err)
	}
	return ec, nil
}

// ZstdExtendedCommitCodec wraps a Raw codec in a zstd compression frame so
// the encoded byte stream stays compact even when ExtendedCommitInfo
// contains hundreds of validators.
type ZstdExtendedCommitCodec struct {
	inner   ExtendedCommitCodec
	encoder *zstd.Encoder
	decoder *zstd.Decoder
}

// NewZstdExtendedCommitCodec wraps `inner` in a zstd frame. The encoder is
// reused across calls — the underlying library is concurrency-safe.
func NewZstdExtendedCommitCodec(inner ExtendedCommitCodec) (*ZstdExtendedCommitCodec, error) {
	enc, err := zstd.NewWriter(nil, zstd.WithEncoderLevel(zstd.SpeedFastest))
	if err != nil {
		return nil, fmt.Errorf("zstd encoder: %w", err)
	}
	dec, err := zstd.NewReader(nil, zstd.WithDecoderConcurrency(0))
	if err != nil {
		return nil, fmt.Errorf("zstd decoder: %w", err)
	}
	return &ZstdExtendedCommitCodec{inner: inner, encoder: enc, decoder: dec}, nil
}

func (c *ZstdExtendedCommitCodec) Encode(ec cometabci.ExtendedCommitInfo) ([]byte, error) {
	raw, err := c.inner.Encode(ec)
	if err != nil {
		return nil, err
	}
	return c.encoder.EncodeAll(raw, nil), nil
}

func (c *ZstdExtendedCommitCodec) Decode(bz []byte) (cometabci.ExtendedCommitInfo, error) {
	if len(bz) == 0 {
		return cometabci.ExtendedCommitInfo{}, nil
	}
	raw, err := c.decoder.DecodeAll(bz, nil)
	if err != nil {
		return cometabci.ExtendedCommitInfo{}, fmt.Errorf("zstd decode: %w", err)
	}
	return c.inner.Decode(raw)
}

// ZstdVoteExtensionCodec wraps a Raw VE codec in a zstd frame. We use this
// for extra-tight VE size budgets (>32 markets per VE).
type ZstdVoteExtensionCodec struct {
	inner   VoteExtensionCodec
	encoder *zstd.Encoder
	decoder *zstd.Decoder
}

// NewZstdVoteExtensionCodec wraps `inner` in a zstd frame.
func NewZstdVoteExtensionCodec(inner VoteExtensionCodec) (*ZstdVoteExtensionCodec, error) {
	enc, err := zstd.NewWriter(nil, zstd.WithEncoderLevel(zstd.SpeedFastest))
	if err != nil {
		return nil, fmt.Errorf("zstd encoder: %w", err)
	}
	dec, err := zstd.NewReader(nil, zstd.WithDecoderConcurrency(0))
	if err != nil {
		return nil, fmt.Errorf("zstd decoder: %w", err)
	}
	return &ZstdVoteExtensionCodec{inner: inner, encoder: enc, decoder: dec}, nil
}

func (c *ZstdVoteExtensionCodec) Encode(v oracletypes.OracleVote) ([]byte, error) {
	raw, err := c.inner.Encode(v)
	if err != nil {
		return nil, err
	}
	return c.encoder.EncodeAll(raw, nil), nil
}

func (c *ZstdVoteExtensionCodec) Decode(bz []byte) (oracletypes.OracleVote, error) {
	if len(bz) == 0 {
		return oracletypes.OracleVote{}, nil
	}
	raw, err := c.decoder.DecodeAll(bz, nil)
	if err != nil {
		return oracletypes.OracleVote{}, fmt.Errorf("zstd decode: %w", err)
	}
	return c.inner.Decode(raw)
}
