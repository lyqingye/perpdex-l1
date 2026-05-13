package types

// DefaultParams returns an empty Params record. The historical fields
// (max_fills_per_msg / max_cancels_per_msg / impact_usdc_amount) have
// been retired; see the proto comment for the migration rationale.
func DefaultParams() Params {
	return Params{}
}

func (Params) Validate() error { return nil }
