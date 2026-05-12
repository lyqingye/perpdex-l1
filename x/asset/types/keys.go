package types

const (
	ModuleName = "asset"
	StoreKey   = ModuleName
	RouterKey  = ModuleName
)

// Collections prefixes for x/asset (matches design doc 11-asset.md §4).
var (
	ParamsKey         = []byte{0x00}
	AssetKey          = []byte{0x01}
	DenomToIndexKey   = []byte{0x02}
	NextAssetIndexKey = []byte{0x03}
)
