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
	return &GenesisState{Params: DefaultParams(), Flags: []LiquidationFlag{}}
}

func (gs GenesisState) Validate() error {
	seen := map[[2]uint64]bool{}
	for _, f := range gs.Flags {
		key := [2]uint64{f.AccountIndex, uint64(f.MarketIndex)}
		if seen[key] {
			return ErrInvalidParams.Wrapf("duplicate liquidation flag (%d,%d)", f.AccountIndex, f.MarketIndex)
		}
		seen[key] = true
	}
	return nil
}
