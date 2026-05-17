package types

import (
	accounttypes "github.com/perpdex/perpdex-l1/x/account/types"
	markettypes "github.com/perpdex/perpdex-l1/x/market/types"
)

// LiquidationRiskSnapshot is the cohesive (account, market) view
// liquidation drives ZP / LLP / ADL math from. All fields are read in
// lockstep and ZeroPrice is derived from them, so the snapshot is the
// only object the decision needs.
//
// Snapshots are immutable values: rebuild after any state mutation
// (fill, collateral move, price refresh) or downstream code will see
// stale TAV/MMR.
//
//   - Risk: targeted envelope — cross aggregate for cross positions,
//     per-position params for isolated. Drives ZP and per-market health.
//   - CrossRisk: always the cross aggregate. ADL ranking uses
//     cross-leverage even for isolated candidates.
//   - ZeroPrice: derived from (Position, MarkPrice, MarketDetails,
//     Risk). Pre-computed so liquidation consumes the value, not the
//     formula inputs.
type LiquidationRiskSnapshot struct {
	Position      accounttypes.AccountPosition
	MarkPrice     uint32
	MarketDetails markettypes.MarketDetails
	Risk          RiskParameters
	CrossRisk     RiskParameters
	ZeroPrice     uint32
}

// ZeroPriceSnapshot is the lightweight (Position, ZeroPrice) bundle
// for non-ADL paths and the gRPC zero-price query. Carries no
// Risk/CrossRisk — callers that need them MUST use
// LiquidationRiskSnapshot instead. Closed positions short-circuit to
// ZeroPrice = 0 without touching the oracle.
type ZeroPriceSnapshot struct {
	Position  accounttypes.AccountPosition
	ZeroPrice uint32
}
