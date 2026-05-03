package types

const (
	// ModuleName is the name of the asset module.
	ModuleName = "asset"
	// StoreKey is the default store key used by x/asset.
	StoreKey = ModuleName
	// RouterKey is the message router key.
	RouterKey = ModuleName
)

// Collections prefixes for x/asset (matches design doc 11-asset.md §4).
var (
	ParamsKey         = []byte{0x00}
	AssetKey          = []byte{0x01}
	DenomToIndexKey   = []byte{0x02}
	NextAssetIndexKey = []byte{0x03}
)
