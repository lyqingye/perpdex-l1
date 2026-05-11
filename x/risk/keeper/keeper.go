package keeper

import (
	"context"
	"fmt"

	"cosmossdk.io/collections"
	"cosmossdk.io/core/store"

	"github.com/cosmos/cosmos-sdk/codec"

	markettypes "github.com/perpdex/perpdex-l1/x/market/types"
	"github.com/perpdex/perpdex-l1/x/risk/types"
)

// Keeper implements the pure risk computations described in 16-risk.md
// and the "Liquidations & LLP" specification. The keeper owns
// only the module Params; pre-state RiskParameters used by the
// post-state regression check live in a function-local
// `types.PreRiskSnapshot` value threaded through by the caller.
//
// The keeper code is split across several files for navigability:
//
//   - keeper.go   : Keeper struct + constructor + universally-shared
//                   helpers (Authority, resolveMarkPrice,
//                   GetMarkAndMarketDetails). Per-RP health classification
//                   lives on RiskParameters itself in x/risk/types so
//                   liquidation-side callers can classify locally without
//                   re-aggregating state.
//   - cross.go    : cross-margin aggregation (ComputeCrossRisk,
//                   GetHealthStatus, GetTotalAccountValue,
//                   GetAvailableCollateral, GetAvailableUsdcCollateral)
//                   and the per-cross half of IsValidRiskChangeFrom.
//   - isolated.go : isolated-margin per-position equivalents
//                   (ComputeIsolatedRisk, GetIsolatedHealthStatus,
//                   IterateIsolatedPositions, isIsolatedRiskChangeValid).
//   - risk_change.go : IsValidRiskChangeFrom, SnapshotRisk, and classifyChange;
//                      stitches cross + isolated together.
//   - liquidation.go : liquidation-specific math
//                      (GetPositionZeroPrice, SimulateRiskAfterTakeover,
//                      GetLiquidationRiskSnapshot, GetZeroPriceSnapshot).
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

// resolveMarkPrice fetches the live mark price for `marketIdx` and
// returns an explicit error in two cases:
//
//   - oracle returns any error (typically StalePrice / ErrNotFound):
//     wrapped as ErrMissingPrice so the caller's regression test can
//     match on err.
//   - mark price is zero: rejected with ErrZeroMarkPrice. A zero mark
//     would silently zero out IM/MM/CM/uPnL and let bankrupt accounts
//     look healthy.
//
// Centralised here to retire the identical guards previously inlined in
// ComputeCrossRisk / ComputeIsolatedRisk / GetPositionZeroPrice /
// SimulateRiskAfterTakeover. Callers that need to attach extra account
// context can wrap the returned error with errors.Wrapf themselves.
func (k Keeper) resolveMarkPrice(ctx context.Context, marketIdx uint32) (uint32, error) {
	px, err := k.oracleKeeper.GetPrice(ctx, marketIdx)
	if err != nil {
		return 0, types.ErrMissingPrice.Wrapf("market=%d: %s", marketIdx, err.Error())
	}
	if px.MarkPrice == 0 {
		return 0, types.ErrZeroMarkPrice.Wrapf("market=%d", marketIdx)
	}
	return px.MarkPrice, nil
}

// GetMarkAndMarketDetails returns the live mark price and `MarketDetails`
// row for `marketIdx` in a single round-trip.
func (k Keeper) GetMarkAndMarketDetails(ctx context.Context, marketIdx uint32) (uint32, markettypes.MarketDetails, error) {
	mark, err := k.resolveMarkPrice(ctx, marketIdx)
	if err != nil {
		return 0, markettypes.MarketDetails{}, err
	}
	md, err := k.marketKeeper.GetMarketDetails(ctx, marketIdx)
	if err != nil {
		return 0, markettypes.MarketDetails{}, err
	}
	return mark, md, nil
}
