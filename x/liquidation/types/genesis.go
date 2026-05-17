package types

// Default ADL caps mirror dYdX v4's MaxDeleveragingAttemptsPerBlock
// (8) and bound the EndBlocker scan with an explicit candidate window.
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
