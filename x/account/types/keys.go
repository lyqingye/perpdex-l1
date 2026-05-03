package types

const (
	// ModuleName is the human-readable identifier of the perpdex account
	// module. It deliberately avoids the prefix "acc" because the SDK auth
	// module already registers a store under that key, and `cosmossdk.io/store`
	// rejects KVStoreKey collections whose names share a common prefix.
	ModuleName = "perpaccount"
	StoreKey   = ModuleName
	RouterKey  = ModuleName
)

// KV store prefixes (see 10-account.md §4).
var (
	ParamsKey         = []byte{0x00}
	AccountKey        = []byte{0x01}
	OwnerToIndexKey   = []byte{0x02}
	MasterSubLinkKey  = []byte{0x03}
	AccountAssetKey   = []byte{0x04}
	AccountPositionKey = []byte{0x05}
	AccountMetaKey    = []byte{0x06}
	NextMasterIndexKey = []byte{0x07}
	NextSubIndexKey    = []byte{0x08}
)
