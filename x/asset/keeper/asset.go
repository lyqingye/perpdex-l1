package keeper

import (
	"context"
	"errors"
	"strings"

	"cosmossdk.io/collections"

	perptypes "github.com/perpdex/perpdex-l1/types"
	"github.com/perpdex/perpdex-l1/x/asset/types"
)

// foldDisplayName normalises a display_name for case-insensitive
// comparison. Defined as a separate helper so the msg path and the
// secondary-index walk stay in lock-step.
func foldDisplayName(name string) string {
	return strings.ToLower(strings.TrimSpace(name))
}

// GetAsset returns the asset registered at index, or ErrAssetNotFound.
func (k Keeper) GetAsset(ctx context.Context, index uint32) (types.Asset, error) {
	a, err := k.Assets.Get(ctx, index)
	if err != nil {
		if errors.Is(err, collections.ErrNotFound) {
			return types.Asset{}, types.ErrAssetNotFound.Wrapf("asset_index=%d", index)
		}
		return types.Asset{}, err
	}
	return a, nil
}

// GetAssetByDenom looks up an asset by its cosmos denom.
//
// The caller is responsible for checking `asset.Enabled` — gRPC reads and
// administrative paths intentionally see disabled assets so they can
// inspect or re-enable them.
func (k Keeper) GetAssetByDenom(ctx context.Context, denom string) (types.Asset, error) {
	idx, err := k.DenomToIndex.Get(ctx, denom)
	if err != nil {
		if errors.Is(err, collections.ErrNotFound) {
			return types.Asset{}, types.ErrAssetNotFound.Wrapf("denom=%s", denom)
		}
		return types.Asset{}, err
	}
	return k.GetAsset(ctx, idx)
}

// SetAsset stores the asset and refreshes the denom index. If the row
// already exists with a different denom, the stale DenomToIndex entry
// is deleted so the secondary index never points at a retired denom.
// Today's MsgUpdateAsset does not expose denom mutation, but keeping
// this invariant local to SetAsset means future writers (governance,
// migrations, tests) cannot accidentally orphan a denom mapping.
func (k Keeper) SetAsset(ctx context.Context, a types.Asset) error {
	if a.AssetIndex == perptypes.NilAssetIndex {
		return types.ErrInvalidAssetParams.Wrap("asset_index must not be nil")
	}
	prev, err := k.Assets.Get(ctx, a.AssetIndex)
	if err != nil && !errors.Is(err, collections.ErrNotFound) {
		return err
	}
	if err == nil && prev.Denom != "" && prev.Denom != a.Denom {
		if err := k.DenomToIndex.Remove(ctx, prev.Denom); err != nil {
			return err
		}
	}
	if err := k.Assets.Set(ctx, a.AssetIndex, a); err != nil {
		return err
	}
	return k.DenomToIndex.Set(ctx, a.Denom, a.AssetIndex)
}

// AllAssets returns every registered asset in asset_index order.
// Used only for genesis export and the (small, bounded) gRPC Assets
// query; pagination-sensitive paths should iterate the collection
// directly.
func (k Keeper) AllAssets(ctx context.Context) ([]types.Asset, error) {
	iter, err := k.Assets.Iterate(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer iter.Close()
	// MaxAssetIndex is a small constant (≤ 62) so a single allocation
	// covers the whole set in production. The slice still grows
	// naturally if a future params bump raises the cap.
	out := make([]types.Asset, 0, perptypes.MaxAssetIndex)
	for ; iter.Valid(); iter.Next() {
		v, err := iter.Value()
		if err != nil {
			return nil, err
		}
		out = append(out, v)
	}
	return out, nil
}

// HasDenom reports whether the denom is already registered.
func (k Keeper) HasDenom(ctx context.Context, denom string) (bool, error) {
	return k.DenomToIndex.Has(ctx, denom)
}

// HasDisplayName reports whether the supplied display_name is already
// in use (case-insensitive, whitespace-trimmed). Walks the Assets
// collection; acceptable because the asset set is bounded by
// `params.max_asset_index` (≤ MaxAssetIndex = 62 in production).
func (k Keeper) HasDisplayName(ctx context.Context, name string) (bool, error) {
	folded := foldDisplayName(name)
	if folded == "" {
		return false, nil
	}
	iter, err := k.Assets.Iterate(ctx, nil)
	if err != nil {
		return false, err
	}
	defer iter.Close()
	for ; iter.Valid(); iter.Next() {
		v, err := iter.Value()
		if err != nil {
			return false, err
		}
		if foldDisplayName(v.DisplayName) == folded {
			return true, nil
		}
	}
	return false, nil
}
