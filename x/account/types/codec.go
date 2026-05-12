package types

import (
	"github.com/cosmos/cosmos-sdk/codec"
	cdctypes "github.com/cosmos/cosmos-sdk/codec/types"
	sdk "github.com/cosmos/cosmos-sdk/types"
	"github.com/cosmos/cosmos-sdk/types/msgservice"
)

func RegisterLegacyAminoCodec(cdc *codec.LegacyAmino) {
	cdc.RegisterConcrete(&MsgDeposit{}, "perpdex/account/MsgDeposit", nil)
	cdc.RegisterConcrete(&MsgWithdraw{}, "perpdex/account/MsgWithdraw", nil)
	cdc.RegisterConcrete(&MsgCreateSubAccount{}, "perpdex/account/MsgCreateSubAccount", nil)
	cdc.RegisterConcrete(&MsgUpdateAccountConfig{}, "perpdex/account/MsgUpdateAccountConfig", nil)
	cdc.RegisterConcrete(&MsgUpdateAccountAssetConfig{}, "perpdex/account/MsgUpdateAccountAssetConfig", nil)
	cdc.RegisterConcrete(&MsgTransfer{}, "perpdex/account/MsgTransfer", nil)
	cdc.RegisterConcrete(&MsgUpdateMargin{}, "perpdex/account/MsgUpdateMargin", nil)
	cdc.RegisterConcrete(&MsgUpdateLeverage{}, "perpdex/account/MsgUpdateLeverage", nil)
	cdc.RegisterConcrete(&MsgUpdateParams{}, "perpdex/account/MsgUpdateParams", nil)
	cdc.RegisterConcrete(&MsgCreatePublicPool{}, "perpdex/account/MsgCreatePublicPool", nil)
	cdc.RegisterConcrete(&MsgUpdatePublicPool{}, "perpdex/account/MsgUpdatePublicPool", nil)
	cdc.RegisterConcrete(&MsgMintShares{}, "perpdex/account/MsgMintShares", nil)
	cdc.RegisterConcrete(&MsgBurnShares{}, "perpdex/account/MsgBurnShares", nil)
	cdc.RegisterConcrete(&MsgStrategyTransfer{}, "perpdex/account/MsgStrategyTransfer", nil)
	cdc.RegisterConcrete(&MsgForceBurnShares{}, "perpdex/account/MsgForceBurnShares", nil)
}

func RegisterInterfaces(reg cdctypes.InterfaceRegistry) {
	reg.RegisterImplementations((*sdk.Msg)(nil),
		&MsgDeposit{},
		&MsgWithdraw{},
		&MsgCreateSubAccount{},
		&MsgUpdateAccountConfig{},
		&MsgUpdateAccountAssetConfig{},
		&MsgTransfer{},
		&MsgUpdateMargin{},
		&MsgUpdateLeverage{},
		&MsgUpdateParams{},
		&MsgCreatePublicPool{},
		&MsgUpdatePublicPool{},
		&MsgMintShares{},
		&MsgBurnShares{},
		&MsgStrategyTransfer{},
		&MsgForceBurnShares{},
	)
	msgservice.RegisterMsgServiceDesc(reg, &_Msg_serviceDesc)
}
