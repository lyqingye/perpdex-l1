package types

import (
	"github.com/cosmos/cosmos-sdk/codec"
	cdctypes "github.com/cosmos/cosmos-sdk/codec/types"
	sdk "github.com/cosmos/cosmos-sdk/types"
	"github.com/cosmos/cosmos-sdk/types/msgservice"
)

func RegisterLegacyAminoCodec(cdc *codec.LegacyAmino) {
	cdc.RegisterConcrete(&MsgCreateMarket{}, "perpdex/market/MsgCreateMarket", nil)
	cdc.RegisterConcrete(&MsgUpdateMarket{}, "perpdex/market/MsgUpdateMarket", nil)
	cdc.RegisterConcrete(&MsgUpdateMarketDetails{}, "perpdex/market/MsgUpdateMarketDetails", nil)
	cdc.RegisterConcrete(&MsgUpdateParams{}, "perpdex/market/MsgUpdateParams", nil)
}

func RegisterInterfaces(reg cdctypes.InterfaceRegistry) {
	reg.RegisterImplementations((*sdk.Msg)(nil),
		&MsgCreateMarket{},
		&MsgUpdateMarket{},
		&MsgUpdateMarketDetails{},
		&MsgUpdateParams{},
	)
	msgservice.RegisterMsgServiceDesc(reg, &_Msg_serviceDesc)
}
