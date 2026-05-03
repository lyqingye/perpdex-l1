package common

import "time"

// USDCDenom is the bank denom for the canonical perp-collateral asset
// registered by the asset module's default genesis.
const USDCDenom = "uusdc"

// DefaultUSDCBalance is the initial bank balance minted to each TestUser
// during SetupTest (1,000,000 USDC at 6-decimal external precision).
const DefaultUSDCBalance uint64 = 1_000_000_000_000

// DefaultBlockStep is the default wall-clock advance per block.
const DefaultBlockStep = time.Second

// HourStep / MinuteStep are convenience aliases used by the funding and
// market-expiry scenarios to land on integer-hour boundaries quickly.
const (
	MinuteStep = time.Minute
	HourStep   = time.Hour
)

// PriceTickToWire converts a "human" price (units of base asset 1.0) into
// the on-chain uint32 representation used by the orderbook. Tests usually
// pick prices that fit comfortably in 32 bits; we just type-cast and let
// the caller assert no overflow.
func PriceTickToWire(price uint64) uint32 { return uint32(price) }
