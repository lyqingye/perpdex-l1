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

// Keeper provides pure trade application functions used by x/matching
// and x/liquidation. It owns no state apart from Params and forwards
// the perp pipeline to a composed `perp.Engine`; spot lives directly on
// the keeper because it is a single account-model path today.
type Keeper struct {
	cdc          codec.BinaryCodec
	storeService store.KVStoreService
	authority    string

	accountKeeper types.AccountKeeper
	marketKeeper  types.MarketKeeper
	fundingKeeper types.FundingKeeper
	riskKeeper    types.RiskKeeper

	// perp encapsulates the cross / isolated (and future unified)
	// perp account-model pipeline. The keeper exposes Apply* methods
	// that thin-forward into it.
	perp perp.Engine

	Schema collections.Schema
	Params collections.Item[types.Params]
}

// PerpFill is the public surface for `Keeper.ApplyPerpsMatching`. It
// is an alias to the engine's Fill struct so external callers
// (matching, liquidation, tests) can keep writing
// `tradekeeper.PerpFill{...}` without taking on a direct dependency
// on the `perp` sub-package.
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
// `perp.Engine.Apply` for the full 8-step pipeline (funding settle,
// pre-risk snapshot, position update, financial routing, treasury +
// liquidation fee, isolated-margin auto-allocation, OI update, post
// risk check).
func (k Keeper) ApplyPerpsMatching(ctx context.Context, f PerpFill) error {
	return k.perp.Apply(ctx, f)
}
