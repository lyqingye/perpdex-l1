package keeper

import (
	"context"

	"github.com/perpdex/perpdex-l1/x/market/types"
)

// InitGenesis seeds Params, every Market (via setMarketWithIndex so
// the ExpiryIndex is fully rebuilt) and every MarketDetails (which
// triggers normalisation on the way in).
//
// GenesisState.Validate has already ensured the Market <-> Details
// pairing is sound, so the loops do not need to cross-check.
func (k Keeper) InitGenesis(ctx context.Context, gs types.GenesisState) error {
	if err := k.Params.Set(ctx, gs.Params); err != nil {
		return err
	}
	for _, m := range gs.Markets {
		if err := k.createMarket(ctx, m); err != nil {
			return err
		}
	}
	for _, d := range gs.MarketDetails {
		if err := k.SetMarketDetails(ctx, d); err != nil {
			return err
		}
	}
	return nil
}

// ExportGenesis snapshots Params + every Market + every MarketDetails.
// Both lists are walked exactly once via collection.Iterate so a
// partial-state corruption surfaces as an error rather than silently
// truncating either list. We also assert pairing parity so the
// resulting genesis is guaranteed re-importable.
func (k Keeper) ExportGenesis(ctx context.Context) (*types.GenesisState, error) {
	p, err := k.Params.Get(ctx)
	if err != nil {
		return nil, err
	}
	markets := []types.Market{}
	if err := k.IterateMarkets(ctx, func(m types.Market) bool {
		markets = append(markets, m)
		return false
	}); err != nil {
		return nil, err
	}
	details := []types.MarketDetails{}
	dIter, err := k.MarketDetails.Iterate(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer dIter.Close()
	for ; dIter.Valid(); dIter.Next() {
		d, err := dIter.Value()
		if err != nil {
			return nil, err
		}
		details = append(details, d)
	}
	if len(markets) != len(details) {
		return nil, types.ErrInvalidMarket.Wrapf(
			"market/details count mismatch: %d markets vs %d details",
			len(markets), len(details),
		)
	}
	return &types.GenesisState{Params: p, Markets: markets, MarketDetails: details}, nil
}
