package types

func DefaultGenesis() *GenesisState {
	return &GenesisState{
		Params:    DefaultParams(),
		Prices:    []OraclePrice{},
		Providers: []OracleProvider{},
		Bindings:  []ValidatorOracleBinding{},
		Stats:     []ValidatorOracleStats{},
	}
}

func (gs GenesisState) Validate() error {
	return gs.Params.Validate()
}
