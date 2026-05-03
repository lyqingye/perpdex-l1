package types

import (
	"github.com/cosmos/cosmos-sdk/codec"
	cdctypes "github.com/cosmos/cosmos-sdk/codec/types"
	sdk "github.com/cosmos/cosmos-sdk/types"
	"github.com/cosmos/cosmos-sdk/types/msgservice"
)

func RegisterLegacyAminoCodec(cdc *codec.LegacyAmino) {
	cdc.RegisterConcrete(&MsgBindOracleOperator{}, "perpdex/oracle/MsgBindOracleOperator", nil)
	cdc.RegisterConcrete(&MsgUnbindOracleOperator{}, "perpdex/oracle/MsgUnbindOracleOperator", nil)
	cdc.RegisterConcrete(&MsgAggregateOracleVotes{}, "perpdex/oracle/MsgAggregateOracleVotes", nil)
	cdc.RegisterConcrete(&MsgInjectOracle{}, "perpdex/oracle/MsgInjectOracle", nil)
	cdc.RegisterConcrete(&MsgAddOracleProvider{}, "perpdex/oracle/MsgAddOracleProvider", nil)
	cdc.RegisterConcrete(&MsgUpdateOracleProvider{}, "perpdex/oracle/MsgUpdateOracleProvider", nil)
	cdc.RegisterConcrete(&MsgSetAggregationMode{}, "perpdex/oracle/MsgSetAggregationMode", nil)
	cdc.RegisterConcrete(&MsgUpdateParams{}, "perpdex/oracle/MsgUpdateParams", nil)
}

func RegisterInterfaces(reg cdctypes.InterfaceRegistry) {
	reg.RegisterImplementations((*sdk.Msg)(nil),
		&MsgBindOracleOperator{},
		&MsgUnbindOracleOperator{},
		&MsgAggregateOracleVotes{},
		&MsgInjectOracle{},
		&MsgAddOracleProvider{},
		&MsgUpdateOracleProvider{},
		&MsgSetAggregationMode{},
		&MsgUpdateParams{},
	)
	msgservice.RegisterMsgServiceDesc(reg, &_Msg_serviceDesc)
}
