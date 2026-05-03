package types

func DefaultGenesis() *GenesisState {
	return &GenesisState{
		Params:   DefaultParams(),
		Metadata: FundingMetadata{},
	}
}

func (gs GenesisState) Validate() error { return gs.Params.Validate() }
