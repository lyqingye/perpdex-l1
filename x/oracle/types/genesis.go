package types

import "fmt"

func DefaultGenesis() *GenesisState {
	return &GenesisState{
		Params: DefaultParams(),
		Prices: []OraclePrice{},
	}
}

func (gs GenesisState) Validate() error {
	if err := gs.Params.Validate(); err != nil {
		return err
	}
	seen := map[uint32]struct{}{}
	for _, p := range gs.Prices {
		if _, dup := seen[p.MarketIndex]; dup {
			return fmt.Errorf("oracle genesis: duplicate market_index %d in prices", p.MarketIndex)
		}
		seen[p.MarketIndex] = struct{}{}
	}
	return nil
}
