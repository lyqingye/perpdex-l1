package keeper

import (
	"context"

	"github.com/perpdex/perpdex-l1/x/oracle/types"
)

// PriceFetcher is the contract between the oracle module and the local
// node's pricing source. It is invoked from `ExtendVote` on every prevote
// to obtain the fresh price set the local validator wants to advertise to
// its peers.
//
// The production implementation lives in `x/oracle/daemon` and reads from
// an in-memory cache populated by a goroutine that polls the local
// oracle-sidecar over gRPC. ABCI handlers therefore never block on a
// sidecar round-trip.
//
// Implementations MUST be safe for concurrent use because cometbft may
// invoke ExtendVote off the main consensus goroutine.
type PriceFetcher interface {
	// FetchPrices returns the latest (mark, index) pair the local oracle
	// daemon currently believes for every market it tracks.
	//
	// Returning an empty slice (no error) is the canonical way for a
	// validator to opt out of a single block — the proposer will weight
	// other validators' votes accordingly.
	FetchPrices(ctx context.Context, height int64) ([]types.MarketPrice, error)
}

// noopPriceFetcher is the default implementation injected by NewKeeper for
// nodes that have not wired a real fetcher (e.g. test chains, full nodes
// that don't run the sidecar). They contribute no price data; the proposer
// will still aggregate the rest of the validator set.
type noopPriceFetcher struct{}

func (noopPriceFetcher) FetchPrices(_ context.Context, _ int64) ([]types.MarketPrice, error) {
	return nil, nil
}

// PriceFetcherFunc adapts a function value to the PriceFetcher interface.
// Convenient for tests and one-off injections from app wiring.
type PriceFetcherFunc func(ctx context.Context, height int64) ([]types.MarketPrice, error)

func (f PriceFetcherFunc) FetchPrices(ctx context.Context, height int64) ([]types.MarketPrice, error) {
	return f(ctx, height)
}
