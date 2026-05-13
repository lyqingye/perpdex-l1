package types

const (
	ModuleName = "liquidation"
	StoreKey   = ModuleName
	RouterKey  = ModuleName
)

var (
	ParamsKey          = []byte{0x00}
	LiquidationFlagKey = []byte{0x01}
)
