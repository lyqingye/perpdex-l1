package types

const (
	ModuleName = "matching"
	StoreKey   = ModuleName
	RouterKey  = ModuleName
)

var (
	ParamsKey       = []byte{0x00}
	ScheduledCancelKey = []byte{0x01}
)
