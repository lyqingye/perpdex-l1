package types

import perptypes "github.com/perpdex/perpdex-l1/types"

func DefaultParams() Params {
	return Params{
		TreasuryAccountIndex:        perptypes.TreasuryAccountIndex,
		InsuranceFundAccountIndex:   perptypes.InsuranceFundOperatorAccountIdx,
		MinPartialTransferAmount:    perptypes.MinPartialTransferAmount,
		MinPartialWithdrawAmount:    perptypes.MinPartialWithdrawAmount,
	}
}

func (p Params) Validate() error {
	if p.MinPartialTransferAmount == 0 {
		return ErrInvalidParams.Wrap("min_partial_transfer_amount must be > 0")
	}
	if p.MinPartialWithdrawAmount == 0 {
		return ErrInvalidParams.Wrap("min_partial_withdraw_amount must be > 0")
	}
	return nil
}
