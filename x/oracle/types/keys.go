package types

const (
	ModuleName = "oracle"
	StoreKey   = ModuleName
	RouterKey  = ModuleName
)

var (
	ParamsKey      = []byte{0x00}
	PriceKey       = []byte{0x01}
	ProviderKey    = []byte{0x02}
	BindingKey     = []byte{0x03}
	OperatorIdxKey = []byte{0x04}
	StatsKey       = []byte{0x05}
	EpochKey       = []byte{0x06}
)
