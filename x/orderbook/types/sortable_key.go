package types

import "encoding/binary"

// SortableKeyLen is the byte length of the orderbook side-sort key:
// 1 byte side discriminator + 4 bytes price + 8 bytes nonce.
const SortableKeyLen = 13

// AskSideByte / BidSideByte are the first byte of every orderbook entry
// key. Asks sort before bids inside a (market, *) prefix iterator; each
// side iterates independently via a `Range.Prefix(market || side)`.
const (
	AskSideByte byte = 0
	BidSideByte byte = 1
)

// SideByte returns the per-side discriminator byte used by SortableKey
// and the side-prefixed range scans in keeper.PeekBestOpposite.
func SideByte(isAsk bool) byte {
	if isAsk {
		return AskSideByte
	}
	return BidSideByte
}

// SortableKey encodes (side, price, nonce) into a 13-byte big-endian
// slice. The layout puts the side byte first so a per-market prefix
// iterator naturally segments into the ask half (side=0) followed by
// the bid half (side=1).
//
// For ask orders the price and nonce are written directly so the
// iterator yields best-first as ascending price / ascending nonce.
// For bid orders we invert both fields (bitwise NOT) so the same
// ascending iterator still yields highest price / lowest nonce first.
//
// Layout (13 bytes):
//   - 1 byte side discriminator (0=ask, 1=bid)
//   - 4 bytes price (BE; bid side stores ^price)
//   - 8 bytes nonce (BE; bid side stores ^uint64(nonce))
func SortableKey(price uint32, nonce int64, isAsk bool) []byte {
	out := make([]byte, SortableKeyLen)
	out[0] = SideByte(isAsk)
	if isAsk {
		binary.BigEndian.PutUint32(out[1:5], price)
		binary.BigEndian.PutUint64(out[5:13], uint64(nonce))
	} else {
		binary.BigEndian.PutUint32(out[1:5], ^price)
		// nonce is in [0, MaxNonce]; invert as uint64 to keep it sortable.
		binary.BigEndian.PutUint64(out[5:13], ^uint64(nonce))
	}
	return out
}

// SidePrefix returns the single-byte prefix used to scope an orderbook
// iteration to one side of a market (combine with the market_index
// prefix via collections.Range.StartInclusive / EndExclusive).
func SidePrefix(isAsk bool) []byte { return []byte{SideByte(isAsk)} }
