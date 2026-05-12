package types

const (
	ModuleName = "market"
	StoreKey   = ModuleName
	RouterKey  = ModuleName
)

// Store key prefixes. These bytes are part of the on-chain ABI: bumping
// them is a breaking state-machine migration. The single-byte
// allocation leaves 0x04..0xFF available for future indices without
// reshaping the layout.
var (
	ParamsKey = []byte{0x00}
	// MarketKey is the prefix for the per-index Market record
	// (collections.Map[uint32, Market]). Holds the static,
	// governance-managed parameters of a perp or spot market.
	MarketKey = []byte{0x01}
	// MarketDetailsKey is the prefix for the per-index MarketDetails
	// record (collections.Map[uint32, MarketDetails]). Holds the
	// runtime / per-block mutable state of a market (mark price,
	// funding sums, open interest, nonces, ...).
	MarketDetailsKey = []byte{0x02}
	// ExpiryIndexKey is the prefix for the secondary index that maps
	// `(expiry_timestamp_ms, market_index)` to nothing
	// (collections.KeySet). EndBlocker walks this set with an upper
	// bound of `now` to find every market that has crossed its
	// ExpiryTimestamp without touching the full Markets table.
	ExpiryIndexKey = []byte{0x03}
)
