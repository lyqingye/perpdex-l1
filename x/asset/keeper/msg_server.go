package keeper

import (
	"context"
	"strconv"

	sdk "github.com/cosmos/cosmos-sdk/types"

	perptypes "github.com/perpdex/perpdex-l1/types"
	"github.com/perpdex/perpdex-l1/x/asset/types"
)

type msgServer struct {
	Keeper
}

// NewMsgServerImpl returns the Msg server for x/asset.
func NewMsgServerImpl(k Keeper) types.MsgServer { return &msgServer{Keeper: k} }

var _ types.MsgServer = msgServer{}

// ensureAuthority rejects any signer that is not the configured
// governance module address. Centralised here so all three handlers
// produce identical, context-bearing error messages.
func (m msgServer) ensureAuthority(signer string) error {
	if signer != m.authority {
		return types.ErrInvalidAuthority.Wrapf("expected %s, got %s", m.authority, signer)
	}
	return nil
}

func (m msgServer) RegisterAsset(ctx context.Context, msg *types.MsgRegisterAsset) (*types.MsgRegisterAssetResponse, error) {
	if err := msg.ValidateBasic(); err != nil {
		return nil, err
	}
	if err := m.ensureAuthority(msg.Authority); err != nil {
		return nil, err
	}

	if exists, err := m.HasDenom(ctx, msg.Denom); err != nil {
		return nil, err
	} else if exists {
		return nil, types.ErrAssetExists.Wrapf("denom=%s", msg.Denom)
	}
	if nameTaken, err := m.HasDisplayName(ctx, msg.DisplayName); err != nil {
		return nil, err
	} else if nameTaken {
		return nil, types.ErrAssetExists.Wrapf("display_name=%s", msg.DisplayName)
	}

	params, err := m.Params.Get(ctx)
	if err != nil {
		return nil, err
	}

	idx, err := m.NextAssetIndex.Next(ctx)
	if err != nil {
		return nil, err
	}
	// The sequence is normalised at InitGenesis to start at
	// max(MinAssetIndex, max_seeded_index+1). We refuse rather than
	// silently bump if a future migration leaves it below the floor —
	// silent bumps mask state corruption and "leak" indices with no
	// audit trail.
	if idx < uint64(perptypes.MinAssetIndex) {
		return nil, types.ErrInvalidModuleParams.Wrapf(
			"next_asset_index=%d below MinAssetIndex=%d; genesis was not normalised",
			idx, perptypes.MinAssetIndex,
		)
	}
	if idx > uint64(params.MaxAssetIndex) {
		return nil, types.ErrAssetIndexExceedsMax.Wrapf("got %d, max %d", idx, params.MaxAssetIndex)
	}
	// Defensive: the USDC slot is reserved for the genesis seed; if the
	// sequence ever advanced past it without seeding USDC the allocator
	// must not hand it out at runtime (which would silently break the
	// "USDC at index 3" invariant).
	if uint32(idx) == perptypes.USDCAssetIndex {
		return nil, types.ErrUSDCMarginConstraint.Wrapf(
			"asset_index=%d is reserved for the genesis-seeded USDC asset",
			perptypes.USDCAssetIndex,
		)
	}

	sdkCtx := sdk.UnwrapSDKContext(ctx)
	asset := types.Asset{
		AssetIndex:          uint32(idx),
		Denom:               msg.Denom,
		DisplayName:         msg.DisplayName,
		Decimals:            msg.Decimals,
		ExtensionMultiplier: msg.ExtensionMultiplier,
		MinTransferAmount:   msg.MinTransferAmount,
		MinWithdrawalAmount: msg.MinWithdrawalAmount,
		MarginMode:          msg.MarginMode, // forced to MarginModeDisabled by ValidateBasic
		Enabled:             true,
		CreatedAt:           sdkCtx.BlockTime().UnixMilli(),
	}
	if err := m.SetAsset(ctx, asset); err != nil {
		return nil, err
	}

	sdkCtx.EventManager().EmitEvent(sdk.NewEvent(
		types.EventTypeAssetRegistered,
		sdk.NewAttribute(types.AttributeKeyAssetIndex, strconv.FormatUint(uint64(asset.AssetIndex), 10)),
		sdk.NewAttribute(types.AttributeKeyDenom, asset.Denom),
		sdk.NewAttribute(types.AttributeKeyDisplayName, asset.DisplayName),
		sdk.NewAttribute(types.AttributeKeyDecimals, strconv.FormatUint(uint64(asset.Decimals), 10)),
		sdk.NewAttribute(types.AttributeKeyExtensionMultiplier, strconv.FormatUint(asset.ExtensionMultiplier, 10)),
		sdk.NewAttribute(types.AttributeKeyMarginMode, strconv.FormatUint(uint64(asset.MarginMode), 10)),
		sdk.NewAttribute(types.AttributeKeyMinTransferAmount, strconv.FormatUint(asset.MinTransferAmount, 10)),
		sdk.NewAttribute(types.AttributeKeyMinWithdrawalAmount, strconv.FormatUint(asset.MinWithdrawalAmount, 10)),
	))

	return &types.MsgRegisterAssetResponse{AssetIndex: asset.AssetIndex}, nil
}

func (m msgServer) UpdateAsset(ctx context.Context, msg *types.MsgUpdateAsset) (*types.MsgUpdateAssetResponse, error) {
	if err := msg.ValidateBasic(); err != nil {
		return nil, err
	}
	if err := m.ensureAuthority(msg.Authority); err != nil {
		return nil, err
	}
	a, err := m.GetAsset(ctx, msg.AssetIndex)
	if err != nil {
		return nil, err
	}
	// USDC underpins every margin operation; disabling it would freeze
	// every collateral deposit/withdrawal on the chain. Refuse here so
	// gov has to take the explicit "halt USDC" code path (not yet
	// exposed) instead of stumbling into it via a routine update.
	if msg.AssetIndex == perptypes.USDCAssetIndex && !msg.Enabled {
		return nil, types.ErrUSDCMarginConstraint.Wrap("USDC must remain enabled")
	}

	a.MinTransferAmount = msg.MinTransferAmount
	a.MinWithdrawalAmount = msg.MinWithdrawalAmount
	a.Enabled = msg.Enabled
	if err := m.SetAsset(ctx, a); err != nil {
		return nil, err
	}

	sdk.UnwrapSDKContext(ctx).EventManager().EmitEvent(sdk.NewEvent(
		types.EventTypeAssetUpdated,
		sdk.NewAttribute(types.AttributeKeyAssetIndex, strconv.FormatUint(uint64(a.AssetIndex), 10)),
		sdk.NewAttribute(types.AttributeKeyDenom, a.Denom),
		sdk.NewAttribute(types.AttributeKeyEnabled, strconv.FormatBool(a.Enabled)),
		sdk.NewAttribute(types.AttributeKeyMinTransferAmount, strconv.FormatUint(a.MinTransferAmount, 10)),
		sdk.NewAttribute(types.AttributeKeyMinWithdrawalAmount, strconv.FormatUint(a.MinWithdrawalAmount, 10)),
	))

	return &types.MsgUpdateAssetResponse{}, nil
}

func (m msgServer) UpdateParams(ctx context.Context, msg *types.MsgUpdateParams) (*types.MsgUpdateParamsResponse, error) {
	if err := msg.ValidateBasic(); err != nil {
		return nil, err
	}
	if err := m.ensureAuthority(msg.Authority); err != nil {
		return nil, err
	}
	if err := m.Params.Set(ctx, msg.Params); err != nil {
		return nil, err
	}

	sdk.UnwrapSDKContext(ctx).EventManager().EmitEvent(sdk.NewEvent(
		types.EventTypeParamsUpdated,
		sdk.NewAttribute(types.AttributeKeyMaxAssetIndex, strconv.FormatUint(uint64(msg.Params.MaxAssetIndex), 10)),
	))

	return &types.MsgUpdateParamsResponse{}, nil
}
