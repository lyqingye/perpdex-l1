package types

func DefaultGenesis() *GenesisState {
	return &GenesisState{
		Params:        DefaultParams(),
		Markets:       []Market{},
		MarketDetails: []MarketDetails{},
	}
}

func (gs GenesisState) Validate() error {
	if err := gs.Params.Validate(); err != nil {
		return err
	}
	seen := map[uint32]bool{}
	for _, m := range gs.Markets {
		if seen[m.MarketIndex] {
			return ErrMarketExists.Wrapf("duplicate market_index %d", m.MarketIndex)
		}
		seen[m.MarketIndex] = true
	}
	return nil
}
