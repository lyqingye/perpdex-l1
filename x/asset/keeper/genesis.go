package keeper

import (
	"context"
	"math"

	perptypes "github.com/perpdex/perpdex-l1/types"
	"github.com/perpdex/perpdex-l1/x/asset/types"
)

// InitGenesis seeds the module from a GenesisState. The caller is
// expected to have already invoked GenesisState.Validate; we still
// normalise the sequence so a missing `next_asset_index` (or one that
// would point at a reserved slot) cannot leave the chain in a state
// where the very first MsgRegisterAsset allocates a nil / sub-minimum
// index.
func (k Keeper) InitGenesis(ctx context.Context, gs types.GenesisState) error {
	if err := k.Params.Set(ctx, gs.Params); err != nil {
		return err
	}
	maxSeenIdx := uint32(0)
	for _, a := range gs.Assets {
		if err := k.SetAsset(ctx, a); err != nil {
			return err
		}
		if a.AssetIndex > maxSeenIdx {
			maxSeenIdx = a.AssetIndex
		}
	}
	// Normalise: the next allocation must be >= MinAssetIndex and
	// strictly greater than every seeded asset_index. We pick the
	// max of (gs.NextAssetIndex, MinAssetIndex, maxSeenIdx+1) so that
	// a fresh-but-empty genesis (NextAssetIndex == 0) still produces a
	// safe starting value.
	next := uint64(gs.NextAssetIndex)
	if min := uint64(perptypes.MinAssetIndex); next < min {
		next = min
	}
	if maxSeenIdx >= perptypes.MinAssetIndex {
		floor := uint64(maxSeenIdx) + 1
		if next < floor {
			next = floor
		}
	}
	return k.NextAssetIndex.Set(ctx, next)
}

// ExportGenesis serialises the module state.
//
// The exported `next_asset_index` is uint32 on the wire; we explicitly
// reject any in-memory sequence value that does not fit so a corrupted
// state can never silently truncate to a still-valid-looking index.
func (k Keeper) ExportGenesis(ctx context.Context) (*types.GenesisState, error) {
	p, err := k.Params.Get(ctx)
	if err != nil {
		return nil, err
	}
	assets, err := k.AllAssets(ctx)
	if err != nil {
		return nil, err
	}
	next, err := k.NextAssetIndex.Peek(ctx)
	if err != nil {
		return nil, err
	}
	if next > math.MaxUint32 || next > uint64(p.MaxAssetIndex)+1 {
		return nil, types.ErrInvalidModuleParams.Wrapf(
			"next_asset_index=%d out of range (max_asset_index=%d)",
			next, p.MaxAssetIndex,
		)
	}
	return &types.GenesisState{Params: p, Assets: assets, NextAssetIndex: uint32(next)}, nil
}
