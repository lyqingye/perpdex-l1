package keeper

import (
	"context"
	"fmt"

	"cosmossdk.io/collections"
	"cosmossdk.io/core/store"

	"github.com/cosmos/cosmos-sdk/codec"

	"github.com/perpdex/perpdex-l1/x/trade/keeper/perp"
	"github.com/perpdex/perpdex-l1/x/trade/types"
)

// Keeper provides pure trade-application functions used by x/matching
// and x/liquidation. Stores only Params; perp work is forwarded to a
// composed perp.Engine, while spot lives directly on the keeper.
type Keeper struct {
	cdc          codec.BinaryCodec
	storeService store.KVStoreService
	authority    string

	accountKeeper types.AccountKeeper
	marketKeeper  types.MarketKeeper
	fundingKeeper types.FundingKeeper
	riskKeeper    types.RiskKeeper

	// perp owns the cross / isolated (and future unified) pipeline.
	perp perp.Engine

	Schema collections.Schema
	Params collections.Item[types.Params]
}

// PerpFill is the public surface for ApplyPerpsMatching, aliasing the
// engine Fill so callers do not depend on the perp sub-package.
type PerpFill = perp.Fill

func NewKeeper(cdc codec.BinaryCodec, storeService store.KVStoreService, authority string,
	ak types.AccountKeeper, mk types.MarketKeeper, fk types.FundingKeeper, rk types.RiskKeeper,
) Keeper {
	sb := collections.NewSchemaBuilder(storeService)
	k := Keeper{
		cdc:           cdc,
		storeService:  storeService,
		authority:     authority,
		accountKeeper: ak,
		marketKeeper:  mk,
		fundingKeeper: fk,
		riskKeeper:    rk,
		perp:          perp.NewEngine(ak, mk, fk, rk),

		Params: collections.NewItem(sb, types.ParamsKey, "params", codec.CollValue[types.Params](cdc)),
	}
	schema, err := sb.Build()
	if err != nil {
		panic(fmt.Errorf("trade: %w", err))
	}
	k.Schema = schema
	return k
}

func (k Keeper) Authority() string { return k.authority }

// ApplyPerpsMatching forwards a perp fill into the engine. See
// perp.Engine.Apply for the 8-step pipeline.
func (k Keeper) ApplyPerpsMatching(ctx context.Context, f PerpFill) error {
	return k.perp.Apply(ctx, f)
}
