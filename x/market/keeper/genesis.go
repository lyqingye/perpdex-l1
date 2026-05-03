package keeper

import (
	"context"

	"github.com/perpdex/perpdex-l1/x/market/types"
)

func (k Keeper) InitGenesis(ctx context.Context, gs types.GenesisState) error {
	if err := k.Params.Set(ctx, gs.Params); err != nil {
		return err
	}
	for _, m := range gs.Markets {
		if err := k.SetMarket(ctx, m); err != nil {
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

func (k Keeper) ExportGenesis(ctx context.Context) (*types.GenesisState, error) {
	p, err := k.Params.Get(ctx)
	if err != nil {
		return nil, err
	}
	markets := []types.Market{}
	if err := k.IterateMarkets(ctx, func(m types.Market) bool { markets = append(markets, m); return false }); err != nil {
		return nil, err
	}
	details := []types.MarketDetails{}
	for _, m := range markets {
		d, err := k.GetMarketDetails(ctx, m.MarketIndex)
		if err == nil {
			details = append(details, d)
		}
	}
	return &types.GenesisState{Params: p, Markets: markets, MarketDetails: details}, nil
}
