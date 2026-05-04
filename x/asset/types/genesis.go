package types

import perptypes "github.com/perpdex/perpdex-l1/types"

// DefaultGenesis returns the default GenesisState for x/asset. The default
// genesis seeds USDC at the canonical asset_index (3) so that perp markets
// have a usable collateral asset out of the box.
func DefaultGenesis() *GenesisState {
	usdc := Asset{
		AssetIndex:          perptypes.USDCAssetIndex,
		Denom:               "uusdc",
		DisplayName:         "USDC",
		Decimals:            6,
		ExtensionMultiplier: perptypes.USDCToCollateralMultiplier,
		MinTransferAmount:   perptypes.MinPartialTransferAmount,
		MinWithdrawalAmount: perptypes.MinPartialWithdrawAmount,
		MarginMode:          perptypes.MarginModeEnabled,
		Enabled:             true,
		CreatedAt:           0,
	}
	return &GenesisState{
		Params:         DefaultParams(),
		Assets:         []Asset{usdc},
		NextAssetIndex: perptypes.USDCAssetIndex + 1,
	}
}

func (gs GenesisState) Validate() error {
	if err := gs.Params.Validate(); err != nil {
		return err
	}
	// NextAssetIndex must not collide with an existing seeded asset; the
	// next allocation otherwise overwrites a pre-existing one.
	seenIndex := map[uint32]bool{}
	seenDenom := map[string]bool{}
	for _, a := range gs.Assets {
		if seenIndex[a.AssetIndex] {
			return ErrAssetExists.Wrapf("duplicate asset_index %d", a.AssetIndex)
		}
		if seenDenom[a.Denom] {
			return ErrAssetExists.Wrapf("duplicate denom %s", a.Denom)
		}
		if a.Denom == "" {
			return ErrInvalidParams.Wrapf("asset_index=%d has empty denom", a.AssetIndex)
		}
		if a.Decimals > 18 {
			return ErrInvalidParams.Wrapf(
				"asset_index=%d decimals=%d exceeds sane max (18)", a.AssetIndex, a.Decimals,
			)
		}
		if a.ExtensionMultiplier == 0 {
			return ErrInvalidParams.Wrapf(
				"asset_index=%d extension_multiplier must be > 0", a.AssetIndex,
			)
		}
		if a.MarginMode != perptypes.MarginModeDisabled &&
			a.MarginMode != perptypes.MarginModeEnabled {
			return ErrInvalidParams.Wrapf(
				"asset_index=%d margin_mode=%d out of range", a.AssetIndex, a.MarginMode,
			)
		}
		seenIndex[a.AssetIndex] = true
		seenDenom[a.Denom] = true
	}
	if gs.NextAssetIndex != 0 && seenIndex[gs.NextAssetIndex] {
		return ErrInvalidParams.Wrapf(
			"next_asset_index=%d collides with seeded asset", gs.NextAssetIndex,
		)
	}
	return nil
}
