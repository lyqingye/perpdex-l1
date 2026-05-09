package types

import (
	accounttypes "github.com/perpdex/perpdex-l1/x/account/types"
	markettypes "github.com/perpdex/perpdex-l1/x/market/types"
)

// LiquidationRiskSnapshot is the cohesive (account, market) view the
// liquidation keeper drives ZP / LLP / ADL math from. All five fields
// are read in lockstep at the time the snapshot is built and `ZeroPrice`
// is computed from those same inputs by the risk keeper, so the
// snapshot is the only object liquidation needs in order to make a
// decision for that pair.
//
// Snapshots are immutable values and MUST be re-built after any state
// mutation (a successful fill, a forced collateral move, a price
// refresh, etc.). Callers that thread one snapshot through a
// state-mutating waterfall WILL feed stale TAV/MMR into the next step
// — refresh per call.
//
// Field semantics:
//
//   - Risk: the position's RELEVANT risk envelope. Cross aggregate
//     for cross positions, per-position parameters for isolated
//     positions. Drives the targeted position's ZP and the per-market
//     liquidation health.
//   - CrossRisk: the account's cross aggregate regardless of margin
//     mode. ADL ranking uses leverage on the cross aggregate per the
//     Lighter spec ("highly-leveraged winners get pulled in first")
//     even when the candidate's targeted position is isolated.
//   - ZeroPrice: the partial-liquidation reference price computed
//     from `(Position, MarkPrice, MarketDetails, Risk)`. Pre-computed
//     here so liquidation never sees the formula's inputs separately
//     — it consumes the value, not the math.
type LiquidationRiskSnapshot struct {
	Position      accounttypes.AccountPosition
	MarkPrice     uint32
	MarketDetails markettypes.MarketDetails
	Risk          RiskParameters
	CrossRisk     RiskParameters
	ZeroPrice     uint32
}
