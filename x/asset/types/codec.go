package types

import (
	"github.com/cosmos/cosmos-sdk/codec"
	"github.com/cosmos/cosmos-sdk/codec/types"
	sdk "github.com/cosmos/cosmos-sdk/types"
	"github.com/cosmos/cosmos-sdk/types/msgservice"
)

// RegisterLegacyAminoCodec registers concrete types on the LegacyAmino codec.
func RegisterLegacyAminoCodec(cdc *codec.LegacyAmino) {
	cdc.RegisterConcrete(&MsgRegisterAsset{}, "perpdex/asset/MsgRegisterAsset", nil)
	cdc.RegisterConcrete(&MsgUpdateAsset{}, "perpdex/asset/MsgUpdateAsset", nil)
	cdc.RegisterConcrete(&MsgUpdateParams{}, "perpdex/asset/MsgUpdateParams", nil)
}

// RegisterInterfaces registers x/asset interface implementations with the
// supplied registry.
func RegisterInterfaces(registry types.InterfaceRegistry) {
	registry.RegisterImplementations((*sdk.Msg)(nil),
		&MsgRegisterAsset{},
		&MsgUpdateAsset{},
		&MsgUpdateParams{},
	)
	msgservice.RegisterMsgServiceDesc(registry, &_Msg_serviceDesc)
}
