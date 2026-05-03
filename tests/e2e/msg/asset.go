// Package msg exposes thin wrappers around each perpdex MsgServer that the
// e2e suite uses to drive the chain. Every helper takes the running
// PerpDEXApp + a writable sdk.Context and returns the response (or an
// error) so that the suite layer can decide how to assert.
//
// The helpers intentionally accept the bare keepers / ctx instead of
// receiving the testify suite — this makes them re-usable from non-suite
// tests and avoids an import cycle between `tests/e2e` and its
// sub-packages.
package msg

import (
	sdk "github.com/cosmos/cosmos-sdk/types"

	perp "github.com/perpdex/perpdex-l1/app"
	perptypes "github.com/perpdex/perpdex-l1/types"
	assetkeeper "github.com/perpdex/perpdex-l1/x/asset/keeper"
	assettypes "github.com/perpdex/perpdex-l1/x/asset/types"
)

// AssetOpts captures all the knobs MsgRegisterAsset accepts; defaults are
// applied where the test doesn't care about a specific value.
type AssetOpts struct {
	Denom               string
	DisplayName         string
	Decimals            uint32
	ExtensionMultiplier uint64
	MinTransferAmount   uint64
	MinWithdrawalAmount uint64
	MarginMode          uint32
}

// Defaults returns a non-margin AssetOpts that mirrors the on-chain
// canonical settings for a 6-decimal external token (e.g. an L2 collateral
// or spot asset). Caller still must override Denom/DisplayName.
func DefaultAssetOpts() AssetOpts {
	return AssetOpts{
		Decimals:            6,
		ExtensionMultiplier: perptypes.USDCToCollateralMultiplier,
		MinTransferAmount:   perptypes.MinPartialTransferAmount,
		MinWithdrawalAmount: perptypes.MinPartialWithdrawAmount,
		MarginMode:          perptypes.MarginModeDisabled,
	}
}

// RegisterAsset dispatches MsgRegisterAsset with `Authority = govAddr`
// directly into the asset MsgServer, bypassing the gov proposal flow. The
// returned asset_index is allocated by the keeper.
func RegisterAsset(
	app *perp.PerpDEXApp,
	ctx sdk.Context,
	govAddr sdk.AccAddress,
	opts AssetOpts,
) (*assettypes.MsgRegisterAssetResponse, error) {
	srv := assetkeeper.NewMsgServerImpl(app.AssetKeeper)
	return srv.RegisterAsset(ctx, &assettypes.MsgRegisterAsset{
		Authority:           govAddr.String(),
		Denom:               opts.Denom,
		DisplayName:         opts.DisplayName,
		Decimals:            opts.Decimals,
		ExtensionMultiplier: opts.ExtensionMultiplier,
		MinTransferAmount:   opts.MinTransferAmount,
		MinWithdrawalAmount: opts.MinWithdrawalAmount,
		MarginMode:          opts.MarginMode,
	})
}

// UpdateAsset toggles the `Enabled` bit and mutates the min-transfer /
// min-withdrawal limits via MsgUpdateAsset. Pass amount=0 to keep the
// existing chain limits.
func UpdateAsset(
	app *perp.PerpDEXApp,
	ctx sdk.Context,
	govAddr sdk.AccAddress,
	assetIndex uint32,
	enabled bool,
	minTransfer, minWithdrawal uint64,
) (*assettypes.MsgUpdateAssetResponse, error) {
	srv := assetkeeper.NewMsgServerImpl(app.AssetKeeper)
	if minTransfer == 0 {
		minTransfer = perptypes.MinPartialTransferAmount
	}
	if minWithdrawal == 0 {
		minWithdrawal = perptypes.MinPartialWithdrawAmount
	}
	return srv.UpdateAsset(ctx, &assettypes.MsgUpdateAsset{
		Authority:           govAddr.String(),
		AssetIndex:          assetIndex,
		MinTransferAmount:   minTransfer,
		MinWithdrawalAmount: minWithdrawal,
		Enabled:             enabled,
	})
}
