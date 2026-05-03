package types

import "encoding/binary"

// SortableKey encodes (price, nonce) into a 12-byte big-endian slice. For ask
// orders we use the values directly so the iterator order is ascending price.
// For bid orders we invert price and nonce so the iterator still yields
// best-first (highest price, then lowest nonce).
//
// Layout (12 bytes):
//   - 4 bytes price (BE)
//   - 8 bytes nonce (BE)
//
// For bids we apply price = 0xFFFFFFFF - price; nonce = MaxUint64 - uint64(nonce).
func SortableKey(price uint32, nonce int64, isAsk bool) []byte {
	out := make([]byte, 12)
	if isAsk {
		binary.BigEndian.PutUint32(out[0:4], price)
		binary.BigEndian.PutUint64(out[4:12], uint64(nonce))
	} else {
		binary.BigEndian.PutUint32(out[0:4], ^price)
		// nonce is in [0, MaxNonce]; invert as uint64 to keep it sortable.
		binary.BigEndian.PutUint64(out[4:12], ^uint64(nonce))
	}
	return out
}

// DecodeSortableKey reads back (price, nonce) from a 12-byte sortable key.
func DecodeSortableKey(key []byte, isAsk bool) (uint32, int64) {
	if len(key) < 12 {
		return 0, 0
	}
	if isAsk {
		return binary.BigEndian.Uint32(key[0:4]), int64(binary.BigEndian.Uint64(key[4:12]))
	}
	return ^binary.BigEndian.Uint32(key[0:4]), int64(^binary.BigEndian.Uint64(key[4:12]))
}
