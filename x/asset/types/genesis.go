package types

import (
	"strings"

	sdk "github.com/cosmos/cosmos-sdk/types"

	perptypes "github.com/perpdex/perpdex-l1/types"
)

// MaxAssetDecimals caps the on-chain `decimals` field. 18 covers every
// EVM-era token (WETH, DAI, ...) without exposing the chain to silly
// values like 255 that downstream math would have to special-case.
const MaxAssetDecimals = uint32(18)

// MaxExtensionMultiplier caps `extension_multiplier`. The internal
// USDC collateral precision is 10^12 (`USDCToCollateralMultiplier =
// 1_000_000`), so 10^18 leaves comfortable headroom for assets that
// extend further while preventing pathologically large multipliers
// that would let a single deposit produce astronomically large
// `math.Int` balances.
const MaxExtensionMultiplier = uint64(1_000_000_000_000_000_000)

// MaxAssetDisplayNameLen caps display_name. Generous enough for normal
// labels (e.g. "USDC", "wstETH") but bounded so the field can't grow
// into a kilobyte memo.
const MaxAssetDisplayNameLen = 32

// canonicalUSDCDisplayName is the reserved display_name for the
// margin-enabled USDC asset. Matching is done case-insensitively after
// trimming whitespace so subtle variants ("usdc", " USDC ", "Usdc")
// cannot be used to register a look-alike.
const canonicalUSDCDisplayName = "USDC"

// canonicalUSDCDenom is the reserved bank denom for USDC. The default
// genesis seeds this denom at `USDCAssetIndex` with margin enabled;
// no other registration path may reuse it.
const canonicalUSDCDenom = "uusdc"

// DefaultGenesis returns the default GenesisState for x/asset. The default
// genesis seeds USDC at the canonical asset_index (3) so that perp markets
// have a usable collateral asset out of the box.
func DefaultGenesis() *GenesisState {
	usdc := Asset{
		AssetIndex:          perptypes.USDCAssetIndex,
		Denom:               canonicalUSDCDenom,
		DisplayName:         canonicalUSDCDisplayName,
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

// IsCanonicalUSDCDisplayName returns true when `name`, after trimming
// surrounding whitespace and case-folding, equals the canonical USDC
// label. The msg path uses this to reject look-alike registrations.
func IsCanonicalUSDCDisplayName(name string) bool {
	return strings.EqualFold(strings.TrimSpace(name), canonicalUSDCDisplayName)
}

// IsCanonicalUSDCDenom returns true when the supplied denom matches the
// reserved USDC bank denom (case-sensitive — bank denoms are exact).
func IsCanonicalUSDCDenom(denom string) bool {
	return denom == canonicalUSDCDenom
}

// validateAssetEntry sanity-checks a single Asset row. Called by
// GenesisState.Validate for every seeded asset and by ValidateBasic
// helpers to keep the genesis path and the msg path on the same
// contract.
func validateAssetEntry(a Asset) error {
	if err := sdk.ValidateDenom(a.Denom); err != nil {
		return ErrInvalidAssetParams.Wrapf("asset_index=%d denom=%q: %s", a.AssetIndex, a.Denom, err.Error())
	}
	if a.DisplayName == "" {
		return ErrInvalidAssetParams.Wrapf("asset_index=%d display_name must be set", a.AssetIndex)
	}
	if len(a.DisplayName) > MaxAssetDisplayNameLen {
		return ErrInvalidAssetParams.Wrapf(
			"asset_index=%d display_name length=%d exceeds max %d",
			a.AssetIndex, len(a.DisplayName), MaxAssetDisplayNameLen,
		)
	}
	if a.Decimals == 0 || a.Decimals > MaxAssetDecimals {
		return ErrInvalidAssetParams.Wrapf(
			"asset_index=%d decimals=%d out of range (1..%d)",
			a.AssetIndex, a.Decimals, MaxAssetDecimals,
		)
	}
	if a.ExtensionMultiplier == 0 || a.ExtensionMultiplier > MaxExtensionMultiplier {
		return ErrInvalidAssetParams.Wrapf(
			"asset_index=%d extension_multiplier=%d out of range (1..%d)",
			a.AssetIndex, a.ExtensionMultiplier, MaxExtensionMultiplier,
		)
	}
	if a.MinTransferAmount == 0 {
		return ErrInvalidAssetParams.Wrapf(
			"asset_index=%d min_transfer_amount must be > 0", a.AssetIndex,
		)
	}
	if a.MinWithdrawalAmount == 0 {
		return ErrInvalidAssetParams.Wrapf(
			"asset_index=%d min_withdrawal_amount must be > 0", a.AssetIndex,
		)
	}
	if a.MarginMode != perptypes.MarginModeDisabled &&
		a.MarginMode != perptypes.MarginModeEnabled {
		return ErrInvalidAssetParams.Wrapf(
			"asset_index=%d margin_mode=%d out of range", a.AssetIndex, a.MarginMode,
		)
	}
	return validateUSDCBinding(a)
}

// validateUSDCBinding enforces the singleton-USDC invariant:
//   - the canonical USDC slot must hold exactly the canonical denom and
//     display_name and be margin-enabled;
//   - every other slot must use a non-canonical denom, non-canonical
//     (case-folded) display_name and be margin-disabled.
//
// Split out so msg validation and genesis validation share one
// definition of "is this row USDC".
func validateUSDCBinding(a Asset) error {
	marginEnabled := a.MarginMode == perptypes.MarginModeEnabled
	if a.AssetIndex == perptypes.USDCAssetIndex {
		if a.Denom != canonicalUSDCDenom {
			return ErrUSDCMarginConstraint.Wrapf(
				"asset_index=%d must use denom %q (got %q)",
				a.AssetIndex, canonicalUSDCDenom, a.Denom,
			)
		}
		if a.DisplayName != canonicalUSDCDisplayName {
			return ErrUSDCMarginConstraint.Wrapf(
				"asset_index=%d must use display_name %q (got %q)",
				a.AssetIndex, canonicalUSDCDisplayName, a.DisplayName,
			)
		}
		if !marginEnabled {
			return ErrUSDCMarginConstraint.Wrapf(
				"asset_index=%d (USDC) must be margin-enabled", a.AssetIndex,
			)
		}
		return nil
	}
	// Non-USDC slots may not impersonate USDC and may not be margin-enabled.
	if marginEnabled {
		return ErrUSDCMarginConstraint.Wrapf(
			"asset_index=%d is margin-enabled but only USDC may be", a.AssetIndex,
		)
	}
	if IsCanonicalUSDCDenom(a.Denom) {
		return ErrUSDCMarginConstraint.Wrapf(
			"denom %q is reserved for the USDC slot", a.Denom,
		)
	}
	if IsCanonicalUSDCDisplayName(a.DisplayName) {
		return ErrUSDCMarginConstraint.Wrapf(
			"display_name %q is reserved for the USDC slot", a.DisplayName,
		)
	}
	return nil
}

// Validate sanity-checks the whole GenesisState. It enforces the
// invariants exercised at boot — duplicate keys, range checks, the USDC
// binding, and that NextAssetIndex sits at a safe value above every
// seeded asset_index.
func (gs GenesisState) Validate() error {
	if err := gs.Params.Validate(); err != nil {
		return err
	}
	maxIdx := gs.Params.MaxAssetIndex

	seenIndex := map[uint32]bool{}
	seenDenom := map[string]bool{}
	seenName := map[string]bool{} // case-folded
	maxSeenAssetIndex := uint32(0)

	for _, a := range gs.Assets {
		if a.AssetIndex < perptypes.MinAssetIndex || a.AssetIndex > maxIdx {
			return ErrInvalidAssetParams.Wrapf(
				"asset_index=%d out of range (%d..%d)",
				a.AssetIndex, perptypes.MinAssetIndex, maxIdx,
			)
		}
		if seenIndex[a.AssetIndex] {
			return ErrAssetExists.Wrapf("duplicate asset_index %d", a.AssetIndex)
		}
		if seenDenom[a.Denom] {
			return ErrAssetExists.Wrapf("duplicate denom %s", a.Denom)
		}
		foldedName := strings.ToLower(strings.TrimSpace(a.DisplayName))
		if seenName[foldedName] {
			return ErrAssetExists.Wrapf("duplicate display_name %s", a.DisplayName)
		}
		if err := validateAssetEntry(a); err != nil {
			return err
		}
		seenIndex[a.AssetIndex] = true
		seenDenom[a.Denom] = true
		seenName[foldedName] = true
		if a.AssetIndex > maxSeenAssetIndex {
			maxSeenAssetIndex = a.AssetIndex
		}
	}

	// NextAssetIndex must point at the next free slot. We allow 0 to
	// mean "uninitialized" — the keeper will normalize it during
	// InitGenesis to max(MinAssetIndex, maxSeenAssetIndex+1). Otherwise
	// it must be in (maxSeenAssetIndex, maxIdx+1] and not collide with
	// any seeded asset.
	if gs.NextAssetIndex == 0 {
		return nil
	}
	if seenIndex[gs.NextAssetIndex] {
		return ErrInvalidModuleParams.Wrapf(
			"next_asset_index=%d collides with seeded asset", gs.NextAssetIndex,
		)
	}
	if gs.NextAssetIndex < perptypes.MinAssetIndex {
		return ErrInvalidModuleParams.Wrapf(
			"next_asset_index=%d below MinAssetIndex=%d",
			gs.NextAssetIndex, perptypes.MinAssetIndex,
		)
	}
	if gs.NextAssetIndex > maxIdx+1 {
		return ErrInvalidModuleParams.Wrapf(
			"next_asset_index=%d above MaxAssetIndex+1=%d",
			gs.NextAssetIndex, maxIdx+1,
		)
	}
	if gs.NextAssetIndex <= maxSeenAssetIndex {
		return ErrInvalidModuleParams.Wrapf(
			"next_asset_index=%d must be > max(asset_index)=%d",
			gs.NextAssetIndex, maxSeenAssetIndex,
		)
	}
	return nil
}
