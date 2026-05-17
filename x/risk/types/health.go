package types

import (
	perptypes "github.com/perpdex/perpdex-l1/types"
)

// HealthStatus is a pure 5-level classifier on a RiskParameters
// envelope, so any holder of one (cross aggregate, isolated params,
// or a snapshot's Risk field) can classify locally without
// re-aggregating state.
//
//	TAV < 0    -> Bankruptcy
//	TAV < CMR  -> FullLiquidation
//	TAV < MMR  -> PartialLiquidation
//	TAV < IMR  -> PreLiquidation
//	otherwise  -> Healthy
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
