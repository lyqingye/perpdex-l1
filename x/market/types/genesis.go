package types

import perptypes "github.com/perpdex/perpdex-l1/types"

func DefaultGenesis() *GenesisState {
	return &GenesisState{
		Params:        DefaultParams(),
		Markets:       []Market{},
		MarketDetails: []MarketDetails{},
	}
}

// Validate enforces genesis-level invariants. The pairing invariant
// (every Market has a matching MarketDetails) is critical so the
// runtime never panics reaching for a missing record.
func (gs GenesisState) Validate() error {
	if err := gs.Params.Validate(); err != nil {
		return err
	}
	seenMarket := map[uint32]bool{}
	for _, m := range gs.Markets {
		if seenMarket[m.MarketIndex] {
			return ErrMarketExists.Wrapf("duplicate market_index %d", m.MarketIndex)
		}
		seenMarket[m.MarketIndex] = true
		if m.Status != perptypes.MarketStatusActive && m.Status != perptypes.MarketStatusExpired {
			return ErrInvalidMarket.Wrapf(
				"market_index=%d status=%d (must be ACTIVE or EXPIRED)",
				m.MarketIndex, m.Status,
			)
		}
		if err := validateMarketStatics(m); err != nil {
			return err
		}
		if !gs.Params.IsValidIndex(m.MarketIndex) {
			return ErrMarketIndexExceed.Wrapf(
				"market_index=%d outside Params range", m.MarketIndex,
			)
		}
	}
	seenDetails := map[uint32]bool{}
	for _, d := range gs.MarketDetails {
		if seenDetails[d.MarketIndex] {
			return ErrInvalidMarket.Wrapf(
				"duplicate market_details for market_index %d", d.MarketIndex,
			)
		}
		seenDetails[d.MarketIndex] = true
		if !seenMarket[d.MarketIndex] {
			return ErrInvalidMarket.Wrapf(
				"market_details references unknown market_index %d", d.MarketIndex,
			)
		}
		if err := validateMarketDetailsStatics(d); err != nil {
			return err
		}
	}
	for idx := range seenMarket {
		if !seenDetails[idx] {
			return ErrInvalidMarket.Wrapf(
				"market_index %d has no matching market_details", idx,
			)
		}
	}
	return nil
}
