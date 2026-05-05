package types

const (
	ModuleName = "oracle"
	StoreKey   = ModuleName
	RouterKey  = ModuleName
)

var (
	ParamsKey = []byte{0x00}
	PriceKey  = []byte{0x01}
)
