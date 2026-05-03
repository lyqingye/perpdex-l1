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
	seenIndex := map[uint32]bool{}
	seenDenom := map[string]bool{}
	for _, a := range gs.Assets {
		if seenIndex[a.AssetIndex] {
			return ErrAssetExists.Wrapf("duplicate asset_index %d", a.AssetIndex)
		}
		if seenDenom[a.Denom] {
			return ErrAssetExists.Wrapf("duplicate denom %s", a.Denom)
		}
		seenIndex[a.AssetIndex] = true
		seenDenom[a.Denom] = true
	}
	return nil
}
