package types

func DefaultGenesis() *GenesisState   { return &GenesisState{Params: Params{}} }
func (gs GenesisState) Validate() error { return nil }
