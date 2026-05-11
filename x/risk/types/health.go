package types

import (
	perptypes "github.com/perpdex/perpdex-l1/types"
)

// HealthStatus implements the 5-level liquidation state machine
// described in the spec. It is a pure function of the
// RiskParameters envelope so callers that already hold an RP value —
// either as the cross aggregate, the isolated per-position
// parameters, or the `Risk` field of a LiquidationRiskSnapshot — can
// classify locally without going through GetHealthStatus /
// GetIsolatedHealthStatus, which would re-aggregate state under the
// hood.
//
// Bands (TAV ordered low to high):
//
//	TAV < 0           -> Bankruptcy
//	TAV < CMR         -> FullLiquidation
//	TAV < MMR         -> PartialLiquidation
//	TAV < IMR         -> PreLiquidation
//	otherwise         -> Healthy
func (p RiskParameters) HealthStatus() uint32 {
	if p.TotalAccountValue.IsNil() {
		return perptypes.HealthHealthy
	}
	if p.TotalAccountValue.IsNegative() {
		return perptypes.HealthBankruptcy
	}
	if !p.CloseOutMarginRequirement.IsNil() && p.TotalAccountValue.LT(p.CloseOutMarginRequirement) {
		return perptypes.HealthFullLiquidation
	}
	if !p.MaintenanceMarginRequirement.IsNil() && p.TotalAccountValue.LT(p.MaintenanceMarginRequirement) {
		return perptypes.HealthPartialLiquidation
	}
	if !p.InitialMarginRequirement.IsNil() && p.TotalAccountValue.LT(p.InitialMarginRequirement) {
		return perptypes.HealthPreLiquidation
	}
	return perptypes.HealthHealthy
}
