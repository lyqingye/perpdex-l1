package types

import (
	"cosmossdk.io/math"

	perptypes "github.com/perpdex/perpdex-l1/types"
)

func DefaultParams() Params {
	return Params{
		TreasuryAccountIndex:          perptypes.TreasuryAccountIndex,
		InsuranceFundAccountIndex:     perptypes.InsuranceFundOperatorAccountIdx,
		MinPartialTransferAmount:      perptypes.MinPartialTransferAmount,
		MinPartialWithdrawAmount:      perptypes.MinPartialWithdrawAmount,
		LiquidityPoolIndex:            perptypes.InsuranceFundOperatorAccountIdx,
		LiquidityPoolCooldownPeriodMs: perptypes.DefaultLLPCooldownPeriodMs,
	}
}

// DefaultInsurancePoolInfo returns the PublicPoolInfo wired into genesis
// for the InsuranceFund account: ACTIVE, no operator fee, no operator
// floor, zero shares (LP money has yet to flow in), strategies all zero.
func DefaultInsurancePoolInfo() *PublicPoolInfo {
	strategies := make([]math.Int, perptypes.NbStrategies)
	for i := range strategies {
		strategies[i] = math.ZeroInt()
	}
	return &PublicPoolInfo{
		Status:               perptypes.PublicPoolStatusActive,
		OperatorFee:          0,
		MinOperatorShareRate: 0,
		TotalShares:          math.ZeroInt(),
		OperatorShares:       math.ZeroInt(),
		Strategies:           strategies,
	}
}

func (p Params) Validate() error {
	if p.MinPartialTransferAmount == 0 {
		return ErrInvalidParams.Wrap("min_partial_transfer_amount must be > 0")
	}
	if p.MinPartialWithdrawAmount == 0 {
		return ErrInvalidParams.Wrap("min_partial_withdraw_amount must be > 0")
	}
	if p.LiquidityPoolCooldownPeriodMs < 0 {
		return ErrInvalidParams.Wrap("liquidity_pool_cooldown_period_ms must be >= 0")
	}
	return nil
}
