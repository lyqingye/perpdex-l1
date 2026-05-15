package types

// Default ADL caps. Picked to match the dYdX v4 defaults of
// MaxDeleveragingAttemptsPerBlock (8) and an explicit candidate window
// per victim so the EndBlocker scan stays bounded.
const (
	DefaultMaxADLAttemptsPerBlock    uint32 = 8
	DefaultMaxADLCandidatesPerVictim uint32 = 16
)

func DefaultParams() Params {
	return Params{
		MaxAdlAttemptsPerBlock:    DefaultMaxADLAttemptsPerBlock,
		MaxAdlCandidatesPerVictim: DefaultMaxADLCandidatesPerVictim,
	}
}

func DefaultGenesis() *GenesisState {
	return &GenesisState{Params: DefaultParams()}
}

func (GenesisState) Validate() error {
	return nil
}
