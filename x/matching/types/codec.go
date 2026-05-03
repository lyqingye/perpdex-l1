package types

import (
	"github.com/cosmos/cosmos-sdk/codec"
	cdctypes "github.com/cosmos/cosmos-sdk/codec/types"
	sdk "github.com/cosmos/cosmos-sdk/types"
	"github.com/cosmos/cosmos-sdk/types/msgservice"
)

func RegisterLegacyAminoCodec(cdc *codec.LegacyAmino) {
	cdc.RegisterConcrete(&MsgCreateOrder{}, "perpdex/matching/MsgCreateOrder", nil)
	cdc.RegisterConcrete(&MsgCancelOrder{}, "perpdex/matching/MsgCancelOrder", nil)
	cdc.RegisterConcrete(&MsgCancelAllOrders{}, "perpdex/matching/MsgCancelAllOrders", nil)
	cdc.RegisterConcrete(&MsgModifyOrder{}, "perpdex/matching/MsgModifyOrder", nil)
	cdc.RegisterConcrete(&MsgUpdateParams{}, "perpdex/matching/MsgUpdateParams", nil)
}

func RegisterInterfaces(reg cdctypes.InterfaceRegistry) {
	reg.RegisterImplementations((*sdk.Msg)(nil),
		&MsgCreateOrder{},
		&MsgCancelOrder{},
		&MsgCancelAllOrders{},
		&MsgModifyOrder{},
		&MsgUpdateParams{},
	)
	msgservice.RegisterMsgServiceDesc(reg, &_Msg_serviceDesc)
}
