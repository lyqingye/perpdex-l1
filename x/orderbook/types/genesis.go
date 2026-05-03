package types

func DefaultGenesis() *GenesisState {
	return &GenesisState{
		Params:         DefaultParams(),
		NextOrderIndex: 1,
		Orders:         []Order{},
	}
}

func (gs GenesisState) Validate() error {
	if err := gs.Params.Validate(); err != nil {
		return err
	}
	seen := map[uint64]bool{}
	for _, o := range gs.Orders {
		if seen[o.OrderIndex] {
			return ErrOrderExists.Wrapf("duplicate order_index %d", o.OrderIndex)
		}
		seen[o.OrderIndex] = true
	}
	return nil
}
