//go:build liveoracle
// +build liveoracle

package e2e_test

import (
	"context"
	"fmt"
	"testing"
	"time"

	abci "github.com/cometbft/cometbft/abci/types"
	cmtproto "github.com/cometbft/cometbft/proto/tendermint/types"

	"cosmossdk.io/log"

	sdk "github.com/cosmos/cosmos-sdk/types"

	perptypes "github.com/perpdex/perpdex-l1/types"
	oraclecodec "github.com/perpdex/perpdex-l1/x/oracle/abci/codec"
	"github.com/perpdex/perpdex-l1/x/oracle/daemon"
	oraclekeeper "github.com/perpdex/perpdex-l1/x/oracle/keeper"

	"github.com/perpdex/perpdex-l1/tests/e2e"
	"github.com/perpdex/perpdex-l1/tests/e2e/msg"
	"github.com/perpdex/perpdex-l1/tests/livehelpers"
)

// OracleLiveSuite drives the real oracle pipeline end-to-end:
//
//	sidecar binary  --> daemon (in-process goroutine, real gRPC) -->
//	PriceFetcher --> ExtendVote --> PrepareProposal injection -->
//	ProcessProposal validation --> PreBlocker weighted-median write
//
// Compared with the unit-style OracleSuite in `e2e_oracle_test.go`, this
// suite removes every mock between the sidecar and the keeper. It is gated
// on the `liveoracle` build tag because it spawns the sidecar binary and
// fetches live prices from Binance / OKX / CoinGecko.
//
//	make build-sidecar && go test -tags liveoracle -count=1 ./tests/e2e/...
//
// Note on the resolver wiring: the production daemon expects the chain's
// markets+assets keepers to be readable from a plain `context.Background()`
// (see `app/app.go:rootCtx`). On the in-memory test app the resolver
// refresh path needs an `sdk.Context` carrying the multistore, so we
// pre-populate the resolver via `Resolver().Set(...)`. This still
// exercises the full daemon → cache → ExtendVote → PreBlock path; the
// resolver's keeper-driven `Refresh` is covered by daemon unit tests.
type OracleLiveSuite struct {
	e2e.PerpdexTestSuite

	BTCAssetIndex uint32
	ETHAssetIndex uint32
	SOLAssetIndex uint32

	BTCMarketIndex uint32
	ETHMarketIndex uint32
	SOLMarketIndex uint32

	veCodec oraclecodec.VoteExtensionCodec
	ecCodec oraclecodec.ExtendedCommitCodec
	handler *oraclekeeper.VoteExtensionHandler

	sidecar *livehelpers.SidecarHandle
	daemon  *daemon.Daemon
}

func TestOracleLiveSuite(t *testing.T) {
	e2e.RunSuite(t, new(OracleLiveSuite))
}

const (
	// chainPriceDecimals must match daemon/adapter.go:defaultPriceDecimals.
	// 2dp keeps BTC * 100 (~ 8.6M) comfortably below the uint32 ceiling.
	chainPriceDecimals uint8 = 2
	sidecarDecimals    uint8 = 8
)

func (s *OracleLiveSuite) SetupTest() {
	s.PerpdexTestSuite.SetupTest()

	s.BTCAssetIndex = s.RegisterAsset(msg.AssetOpts{
		Denom: "ubtc", DisplayName: "BTC", Decimals: 8,
		ExtensionMultiplier: 1, MinTransferAmount: 1, MinWithdrawalAmount: 1,
		MarginMode: perptypes.MarginModeDisabled,
	})
	s.ETHAssetIndex = s.RegisterAsset(msg.AssetOpts{
		Denom: "ueth", DisplayName: "ETH", Decimals: 18,
		ExtensionMultiplier: 1, MinTransferAmount: 1, MinWithdrawalAmount: 1,
		MarginMode: perptypes.MarginModeDisabled,
	})
	s.SOLAssetIndex = s.RegisterAsset(msg.AssetOpts{
		Denom: "usol", DisplayName: "SOL", Decimals: 9,
		ExtensionMultiplier: 1, MinTransferAmount: 1, MinWithdrawalAmount: 1,
		MarginMode: perptypes.MarginModeDisabled,
	})

	s.BTCMarketIndex = s.CreatePerpMarket(msg.DefaultPerpMarketOpts(1, s.BTCAssetIndex))
	s.ETHMarketIndex = s.CreatePerpMarket(msg.DefaultPerpMarketOpts(2, s.ETHAssetIndex))
	s.SOLMarketIndex = s.CreatePerpMarket(msg.DefaultPerpMarketOpts(3, s.SOLAssetIndex))

	s.veCodec = oraclecodec.NewRawVoteExtensionCodec()
	s.ecCodec = oraclecodec.NewRawExtendedCommitCodec()
	s.handler = oraclekeeper.NewVoteExtensionHandler(s.App.OracleKeeper, s.veCodec, s.ecCodec)

	s.sidecar = livehelpers.StartSidecar(s.T(), livehelpers.DefaultLiveConfig(), 15*time.Second)

	d := daemon.New(log.NewTestLogger(s.T()), daemon.Config{
		SidecarAddress:  s.sidecar.GRPCAddr,
		FetchInterval:   200 * time.Millisecond,
		FetchTimeout:    1 * time.Second,
		SidecarDecimals: sidecarDecimals,
		MaxAge:          5 * time.Second,
		Enabled:         true,
	}, nil, nil)
	d.Resolver().Set("BTC/USD", s.BTCMarketIndex, chainPriceDecimals)
	d.Resolver().Set("ETH/USD", s.ETHMarketIndex, chainPriceDecimals)
	d.Resolver().Set("SOL/USD", s.SOLMarketIndex, chainPriceDecimals)
	s.Require().NoError(d.Start(context.Background()))
	s.daemon = d
	s.T().Cleanup(d.Stop)

	s.App.OracleKeeper.SetPriceFetcher(d.AsPriceFetcher())

	// Wait until the daemon has a fresh observation for every market we
	// care about. Without this, ExtendVote on the first block we drive
	// would race against the first poll tick and produce an empty
	// extension.
	s.Require().NoError(livehelpers.WaitFor(context.Background(), 15*time.Second, func() error {
		snap := d.Cache().Snapshot(time.Now().UTC(), 5*time.Second)
		if len(snap) < 3 {
			return fmt.Errorf("only %d markets cached, want 3", len(snap))
		}
		seen := map[uint32]bool{}
		for _, p := range snap {
			if p.IndexPrice == 0 {
				return fmt.Errorf("market %d still zero", p.MarketIndex)
			}
			seen[p.MarketIndex] = true
		}
		for _, idx := range []uint32{s.BTCMarketIndex, s.ETHMarketIndex, s.SOLMarketIndex} {
			if !seen[idx] {
				return fmt.Errorf("market_index %d missing", idx)
			}
		}
		return nil
	}))
}

func (s *OracleLiveSuite) veCtx() sdk.Context {
	return s.Ctx.WithConsensusParams(cmtproto.ConsensusParams{
		Abci: &cmtproto.ABCIParams{VoteExtensionsEnableHeight: 1},
	})
}

// TestPipelineAggregatesLivePrices exercises every ABCI++ stage with the
// real sidecar feeding the daemon. We inject the daemon's vote extension
// for one validator (single-vote 100% supermajority is fine for the
// in-memory app) and assert that PreBlocker writes a non-zero IndexPrice
// for each market that the sidecar quotes.
func (s *OracleLiveSuite) TestPipelineAggregatesLivePrices() {
	ctx := s.veCtx()

	// 1. ExtendVote must produce a payload that decodes to all three
	//    markets with non-zero prices.
	ext, err := s.handler.ExtendVote()(ctx, &abci.RequestExtendVote{Height: 5})
	s.Require().NoError(err)
	s.Require().NotEmpty(ext.VoteExtension)

	decoded, err := s.veCodec.Decode(ext.VoteExtension)
	s.Require().NoError(err)
	s.Require().Len(decoded.Prices, 3, "ExtendVote should reflect all three sidecar markets")
	for _, p := range decoded.Prices {
		s.Require().NotZero(p.IndexPrice)
		s.Require().NotZero(p.MarkPrice)
	}

	// 2. VerifyVoteExtension on our own payload (the chain runs this on
	//    every peer's VE in production).
	verify, err := s.handler.VerifyVoteExtension()(ctx, &abci.RequestVerifyVoteExtension{
		Height: 5, VoteExtension: ext.VoteExtension,
	})
	s.Require().NoError(err)
	s.Require().Equal(abci.ResponseVerifyVoteExtension_ACCEPT, verify.Status)

	// 3. PrepareProposal: wrap a single committed vote into an
	//    ExtendedCommitInfo and let the handler prepend it as Txs[0].
	commitInfo := abci.ExtendedCommitInfo{
		Round: 0,
		Votes: []abci.ExtendedVoteInfo{{
			Validator:          abci.Validator{Address: []byte("validator-aaaaaaaa"), Power: 100},
			BlockIdFlag:        cmtproto.BlockIDFlagCommit,
			VoteExtension:      ext.VoteExtension,
			ExtensionSignature: []byte("sig-1"),
		}},
	}
	wrapped := func(_ sdk.Context, req *abci.RequestPrepareProposal) (*abci.ResponsePrepareProposal, error) {
		return &abci.ResponsePrepareProposal{Txs: req.Txs}, nil
	}
	prep, err := s.handler.PrepareProposal(wrapped)(ctx, &abci.RequestPrepareProposal{
		Height:          6,
		LocalLastCommit: commitInfo,
		MaxTxBytes:      1024 * 1024,
	})
	s.Require().NoError(err)
	s.Require().NotEmpty(prep.Txs)

	// 4. ProcessProposal: 100% commit power passes the supermajority gate.
	procResp, err := s.handler.ProcessProposal(func(_ sdk.Context, _ *abci.RequestProcessProposal) (*abci.ResponseProcessProposal, error) {
		return &abci.ResponseProcessProposal{Status: abci.ResponseProcessProposal_ACCEPT}, nil
	})(ctx, &abci.RequestProcessProposal{Height: 6, Txs: prep.Txs})
	s.Require().NoError(err)
	s.Require().Equal(abci.ResponseProcessProposal_ACCEPT, procResp.Status)

	// 5. Disable mark smoothing so we can assert directly against the VE
	//    payload without an EMA blend.
	params, err := s.App.OracleKeeper.Params.Get(ctx)
	s.Require().NoError(err)
	params.MarkPriceEmaAlpha = 0
	s.Require().NoError(s.App.OracleKeeper.Params.Set(ctx, params))

	preCtx := ctx.WithBlockHeight(6).WithBlockTime(s.BlockTime())
	_, err = s.handler.PreBlocker()(preCtx, &abci.RequestFinalizeBlock{
		Height: 6, Txs: prep.Txs, Time: s.BlockTime(),
	})
	s.Require().NoError(err)

	// 6. Assert all three markets received non-zero, in-the-right-ballpark
	//    prices. Lower bounds intentionally generous so live market moves
	//    don't make the test brittle.
	expectedLowerBounds := map[uint32]uint32{
		s.BTCMarketIndex: 100_000, // BTC > $1000 in 2dp units
		s.ETHMarketIndex: 10_000,  // ETH > $100
		s.SOLMarketIndex: 100,     // SOL > $1
	}
	for marketIdx, low := range expectedLowerBounds {
		got, err := s.App.OracleKeeper.GetPrice(preCtx, marketIdx)
		s.Require().NoError(err, "GetPrice(market %d)", marketIdx)
		s.Require().Greater(got.IndexPrice, low,
			"market %d: index price %d should exceed lower bound %d", marketIdx, got.IndexPrice, low)
		s.Require().Greater(got.MarkPrice, low,
			"market %d: mark price %d should exceed lower bound %d", marketIdx, got.MarkPrice, low)
		s.Require().Equal(int64(6), got.LastUpdatedHeight)
		s.Require().NotZero(got.LastUpdatedTimestamp)
	}
}
