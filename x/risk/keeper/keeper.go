package keeper

import (
	"context"
	"fmt"

	"cosmossdk.io/collections"
	"cosmossdk.io/core/store"

	"github.com/cosmos/cosmos-sdk/codec"
	sdk "github.com/cosmos/cosmos-sdk/types"

	markettypes "github.com/perpdex/perpdex-l1/x/market/types"
	"github.com/perpdex/perpdex-l1/x/risk/types"
)

// Keeper implements the pure risk computations described in 16-risk.md
// and the "Liquidations & LLP" specification. The keeper owns only the
// module Params; pre-state RiskParameters used by the post-state
// regression check live in a function-local `types.PreRiskSnapshot`
// value threaded through by the caller.
//
// Schema byte prefixes 0x01 / 0x02 were used for the now-removed
// pre-state KV caches; future schema additions MUST pick a fresh
// byte to avoid colliding with any historical state.
type Keeper struct {
	cdc           codec.BinaryCodec
	storeService  store.KVStoreService
	authority     string
	accountKeeper types.AccountKeeper
	marketKeeper  types.MarketKeeper
	oracleKeeper  types.OracleKeeper
	fundingKeeper types.FundingKeeper

	Schema collections.Schema
	Params collections.Item[types.Params]
}

func NewKeeper(cdc codec.BinaryCodec, storeService store.KVStoreService, authority string,
	ak types.AccountKeeper, mk types.MarketKeeper, ok types.OracleKeeper,
) Keeper {
	sb := collections.NewSchemaBuilder(storeService)
	k := Keeper{
		cdc:           cdc,
		storeService:  storeService,
		authority:     authority,
		accountKeeper: ak,
		marketKeeper:  mk,
		oracleKeeper:  ok,

		Params: collections.NewItem(sb, types.ParamsKey, "params", codec.CollValue[types.Params](cdc)),
	}
	schema, err := sb.Build()
	if err != nil {
		panic(fmt.Errorf("risk: %w", err))
	}
	k.Schema = schema
	return k
}

func (k Keeper) Authority() string { return k.authority }

// SetFundingKeeper wires the funding keeper after construction. Required
// so `resolveMarkPrice` can gate on `Funding.Params.MaxMarkStalenessMs`.
// Late binding avoids the import cycle that would otherwise arise from
// x/funding depending on x/risk for the liquidation gate (cancel-all in
// risk transitions) and x/risk depending on x/funding for the staleness
// param.
func (k *Keeper) SetFundingKeeper(f types.FundingKeeper) { k.fundingKeeper = f }

// gateMarkPrice validates that `d.MarkPrice` is fresh and non-zero.
// Returns an explicit error in three cases (fail-closed):
//
//   - `d.MarkPrice == 0`: rejected with ErrZeroMarkPrice. A zero mark
//     would silently zero out IM/MM/CM/uPnL and let bankrupt accounts
//     look healthy.
//   - mark price is stale relative to `Funding.Params.MaxMarkStalenessMs`
//     (the funding BeginBlocker has not refreshed it within the
//     governance-configured window): wrapped as ErrMissingPrice. A
//     stale-but-non-zero mark could cause a risk read to honour a
//     price that no longer reflects reality.
//   - the funding keeper read for `MaxMarkStalenessMs` fails: wrapped
//     as ErrMissingPrice.
//
// When the funding keeper has not been wired (legacy callers, tests
// that forgot SetFundingKeeper) the staleness gate is bypassed —
// behaviour matches the pre-median-mark codepath. App wiring at
// app/keepers/keepers.go MUST call SetFundingKeeper before handing
// the risk keeper to Trade/Matching/Liquidation so every consumer
// observes the gate.
func (k Keeper) gateMarkPrice(ctx context.Context, marketIdx uint32, d markettypes.MarketDetails) error {
	if d.MarkPrice == 0 {
		return types.ErrZeroMarkPrice.Wrapf("market=%d", marketIdx)
	}
	if k.fundingKeeper == nil {
		return nil
	}
	maxStaleness, err := k.fundingKeeper.MaxMarkStalenessMs(ctx)
	if err != nil {
		return types.ErrMissingPrice.Wrapf("market=%d: funding params: %s", marketIdx, err.Error())
	}
	if maxStaleness <= 0 {
		return nil
	}
	now := sdk.UnwrapSDKContext(ctx).BlockTime().UnixMilli()
	if d.LastMarkPriceTimestamp == 0 || now-d.LastMarkPriceTimestamp > maxStaleness {
		return types.ErrMissingPrice.Wrapf(
			"market=%d: mark stale, last_update_ms=%d now_ms=%d max_staleness_ms=%d",
			marketIdx, d.LastMarkPriceTimestamp, now, maxStaleness,
		)
	}
	return nil
}

// resolveMarkPrice fetches the live mark price for `marketIdx` from
// `MarketDetails.MarkPrice` (the chain's authoritative mark, written
// every block by the funding BeginBlocker as median(impact_mid,
// index + premium_ema, oracle_mark)) gated by `gateMarkPrice`.
func (k Keeper) resolveMarkPrice(ctx context.Context, marketIdx uint32) (uint32, error) {
	d, err := k.marketKeeper.GetMarketDetails(ctx, marketIdx)
	if err != nil {
		return 0, types.ErrMissingPrice.Wrapf("market=%d: %s", marketIdx, err.Error())
	}
	if err := k.gateMarkPrice(ctx, marketIdx, d); err != nil {
		return 0, err
	}
	return d.MarkPrice, nil
}

// GetMarkAndMarketDetails returns the live mark price and `MarketDetails`
// row for `marketIdx` in a single round-trip, applying the same
// staleness / zero gate as `resolveMarkPrice`.
func (k Keeper) GetMarkAndMarketDetails(ctx context.Context, marketIdx uint32) (uint32, markettypes.MarketDetails, error) {
	md, err := k.marketKeeper.GetMarketDetails(ctx, marketIdx)
	if err != nil {
		return 0, markettypes.MarketDetails{}, types.ErrMissingPrice.Wrapf("market=%d: %s", marketIdx, err.Error())
	}
	if err := k.gateMarkPrice(ctx, marketIdx, md); err != nil {
		return 0, markettypes.MarketDetails{}, err
	}
	return md.MarkPrice, md, nil
}
