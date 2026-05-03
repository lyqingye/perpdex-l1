package types

const (
	ModuleName = "market"
	StoreKey   = ModuleName
	RouterKey  = ModuleName
)

var (
	ParamsKey         = []byte{0x00}
	MarketKey         = []byte{0x01}
	MarketDetailsKey  = []byte{0x02}
	ExpiryIndexKey    = []byte{0x03}
)
