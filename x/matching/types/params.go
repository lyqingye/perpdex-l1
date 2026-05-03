package types

func DefaultParams() Params {
	return Params{
		MaxFillsPerMsg:   100,
		MaxCancelsPerMsg: 200,
	}
}

func (p Params) Validate() error {
	if p.MaxFillsPerMsg == 0 {
		return ErrInvalidParams.Wrap("max_fills_per_msg must be > 0")
	}
	if p.MaxCancelsPerMsg == 0 {
		return ErrInvalidParams.Wrap("max_cancels_per_msg must be > 0")
	}
	return nil
}
