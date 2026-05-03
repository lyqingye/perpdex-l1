package types

import (
	"github.com/cosmos/cosmos-sdk/codec"
	cdctypes "github.com/cosmos/cosmos-sdk/codec/types"
)

// x/orderbook does not expose any user-callable Msg type — all order mutations
// flow through x/matching. We still provide the standard registration hooks
// to keep wiring uniform.
func RegisterLegacyAminoCodec(_ *codec.LegacyAmino) {}

func RegisterInterfaces(_ cdctypes.InterfaceRegistry) {}
