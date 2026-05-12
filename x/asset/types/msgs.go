package types

import (
	sdk "github.com/cosmos/cosmos-sdk/types"
	sdkerrors "github.com/cosmos/cosmos-sdk/types/errors"

	perptypes "github.com/perpdex/perpdex-l1/types"
)

var (
	_ sdk.Msg = (*MsgRegisterAsset)(nil)
	_ sdk.Msg = (*MsgUpdateAsset)(nil)
	_ sdk.Msg = (*MsgUpdateParams)(nil)
)

// ValidateBasic enforces shape-only constraints on MsgRegisterAsset.
// It deliberately rejects any margin-enabled / USDC-look-alike payload:
// the canonical USDC row is seeded by genesis and cannot be introduced
// at runtime, which keeps the "USDC is the unique collateral" invariant
// outside the reach of governance accidents.
func (m *MsgRegisterAsset) ValidateBasic() error {
	if _, err := sdk.AccAddressFromBech32(m.Authority); err != nil {
		return sdkerrors.ErrInvalidAddress.Wrapf("authority: %s", err)
	}
	if err := sdk.ValidateDenom(m.Denom); err != nil {
		return ErrInvalidAssetParams.Wrapf("denom=%q: %s", m.Denom, err.Error())
	}
	if IsCanonicalUSDCDenom(m.Denom) {
		return ErrUSDCMarginConstraint.Wrap("denom \"uusdc\" is reserved for the genesis-seeded USDC asset")
	}
	if m.DisplayName == "" {
		return ErrInvalidAssetParams.Wrap("display_name must be set")
	}
	if len(m.DisplayName) > MaxAssetDisplayNameLen {
		return ErrInvalidAssetParams.Wrapf(
			"display_name length=%d exceeds max %d", len(m.DisplayName), MaxAssetDisplayNameLen,
		)
	}
	if IsCanonicalUSDCDisplayName(m.DisplayName) {
		return ErrUSDCMarginConstraint.Wrap("display_name \"USDC\" is reserved for the genesis-seeded USDC asset")
	}
	if m.Decimals == 0 || m.Decimals > MaxAssetDecimals {
		return ErrInvalidAssetParams.Wrapf(
			"decimals=%d out of range (1..%d)", m.Decimals, MaxAssetDecimals,
		)
	}
	if m.ExtensionMultiplier == 0 || m.ExtensionMultiplier > MaxExtensionMultiplier {
		return ErrInvalidAssetParams.Wrapf(
			"extension_multiplier=%d out of range (1..%d)",
			m.ExtensionMultiplier, MaxExtensionMultiplier,
		)
	}
	if m.MinTransferAmount == 0 {
		return ErrInvalidAssetParams.Wrap("min_transfer_amount must be > 0")
	}
	if m.MinWithdrawalAmount == 0 {
		return ErrInvalidAssetParams.Wrap("min_withdrawal_amount must be > 0")
	}
	if m.MarginMode != perptypes.MarginModeDisabled {
		return ErrUSDCMarginConstraint.Wrap("margin-enabled assets are seeded by genesis only")
	}
	return nil
}

// ValidateBasic enforces shape-only constraints on MsgUpdateAsset.
// Note: callers must always supply non-zero min_transfer_amount and
// min_withdrawal_amount even when only toggling `enabled`; this keeps
// the schema simple at the cost of a small caller-side hop (read the
// current asset, then echo those fields back).
func (m *MsgUpdateAsset) ValidateBasic() error {
	if _, err := sdk.AccAddressFromBech32(m.Authority); err != nil {
		return sdkerrors.ErrInvalidAddress.Wrapf("authority: %s", err)
	}
	if m.AssetIndex == perptypes.NilAssetIndex {
		return ErrInvalidAssetParams.Wrap("asset_index must not be nil")
	}
	if m.MinTransferAmount == 0 {
		return ErrInvalidAssetParams.Wrap("min_transfer_amount must be > 0")
	}
	if m.MinWithdrawalAmount == 0 {
		return ErrInvalidAssetParams.Wrap("min_withdrawal_amount must be > 0")
	}
	return nil
}

func (m *MsgUpdateParams) ValidateBasic() error {
	if _, err := sdk.AccAddressFromBech32(m.Authority); err != nil {
		return sdkerrors.ErrInvalidAddress.Wrapf("authority: %s", err)
	}
	return m.Params.Validate()
}
