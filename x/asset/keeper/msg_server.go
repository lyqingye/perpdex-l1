package keeper

import (
	"context"

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

func (m msgServer) RegisterAsset(ctx context.Context, msg *types.MsgRegisterAsset) (*types.MsgRegisterAssetResponse, error) {
	if msg.Authority != m.authority {
		return nil, types.ErrInvalidAuthority.Wrapf("expected %s, got %s", m.authority, msg.Authority)
	}
	if err := msg.ValidateBasic(); err != nil {
		return nil, err
	}

	exists, err := m.HasDenom(ctx, msg.Denom)
	if err != nil {
		return nil, err
	}
	if exists {
		return nil, types.ErrAssetExists.Wrapf("denom=%s", msg.Denom)
	}

	params, err := m.Params.Get(ctx)
	if err != nil {
		return nil, err
	}

	idx, err := m.NextAssetIndex.Next(ctx)
	if err != nil {
		return nil, err
	}
	if idx < uint64(perptypes.MinAssetIndex) {
		// Bootstrap: initial seq is 0, bump until >= MinAssetIndex
		for idx < uint64(perptypes.MinAssetIndex) {
			idx, err = m.NextAssetIndex.Next(ctx)
			if err != nil {
				return nil, err
			}
		}
	}
	if idx > uint64(params.MaxAssetIndex) {
		return nil, types.ErrAssetIndexExceedsMax.Wrapf("got %d, max %d", idx, params.MaxAssetIndex)
	}

	// USDC <-> margin enabled invariant. We treat display_name == "USDC" or
	// asset_index == 3 (USDC_ASSET_INDEX) as the USDC binding.
	isUSDC := msg.DisplayName == "USDC" || uint32(idx) == perptypes.USDCAssetIndex
	isMarginEnabled := msg.MarginMode == perptypes.MarginModeEnabled
	if isUSDC != isMarginEnabled {
		return nil, types.ErrUSDCMarginConstraint
	}

	asset := types.Asset{
		AssetIndex:          uint32(idx),
		Denom:               msg.Denom,
		DisplayName:         msg.DisplayName,
		Decimals:            msg.Decimals,
		ExtensionMultiplier: msg.ExtensionMultiplier,
		MinTransferAmount:   msg.MinTransferAmount,
		MinWithdrawalAmount: msg.MinWithdrawalAmount,
		MarginMode:          msg.MarginMode,
		Enabled:             true,
		CreatedAt:           sdk.UnwrapSDKContext(ctx).BlockTime().UnixMilli(),
	}
	if err := m.SetAsset(ctx, asset); err != nil {
		return nil, err
	}

	sdk.UnwrapSDKContext(ctx).EventManager().EmitEvent(sdk.NewEvent(
		"asset_registered",
		sdk.NewAttribute("asset_index", uintToString(uint64(idx))),
		sdk.NewAttribute("denom", msg.Denom),
	))

	return &types.MsgRegisterAssetResponse{AssetIndex: uint32(idx)}, nil
}

func (m msgServer) UpdateAsset(ctx context.Context, msg *types.MsgUpdateAsset) (*types.MsgUpdateAssetResponse, error) {
	if msg.Authority != m.authority {
		return nil, types.ErrInvalidAuthority
	}
	if err := msg.ValidateBasic(); err != nil {
		return nil, err
	}
	a, err := m.GetAsset(ctx, msg.AssetIndex)
	if err != nil {
		return nil, err
	}
	a.MinTransferAmount = msg.MinTransferAmount
	a.MinWithdrawalAmount = msg.MinWithdrawalAmount
	a.Enabled = msg.Enabled
	if err := m.SetAsset(ctx, a); err != nil {
		return nil, err
	}
	return &types.MsgUpdateAssetResponse{}, nil
}

func (m msgServer) UpdateParams(ctx context.Context, msg *types.MsgUpdateParams) (*types.MsgUpdateParamsResponse, error) {
	if msg.Authority != m.authority {
		return nil, types.ErrInvalidAuthority
	}
	if err := msg.Params.Validate(); err != nil {
		return nil, err
	}
	if err := m.Params.Set(ctx, msg.Params); err != nil {
		return nil, err
	}
	return &types.MsgUpdateParamsResponse{}, nil
}

func uintToString(u uint64) string {
	if u == 0 {
		return "0"
	}
	var b [20]byte
	i := len(b)
	for u > 0 {
		i--
		b[i] = byte('0' + u%10)
		u /= 10
	}
	return string(b[i:])
}
