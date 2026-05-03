package keeper

import (
	"context"
	"errors"

	"cosmossdk.io/collections"

	"github.com/perpdex/perpdex-l1/x/asset/types"
)

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

// SetAsset stores the asset and refreshes the denom index.
func (k Keeper) SetAsset(ctx context.Context, a types.Asset) error {
	if err := k.Assets.Set(ctx, a.AssetIndex, a); err != nil {
		return err
	}
	return k.DenomToIndex.Set(ctx, a.Denom, a.AssetIndex)
}

// AllAssets returns every registered asset (no pagination).
func (k Keeper) AllAssets(ctx context.Context) ([]types.Asset, error) {
	iter, err := k.Assets.Iterate(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer iter.Close()
	out := []types.Asset{}
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
