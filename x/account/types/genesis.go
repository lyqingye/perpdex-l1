package types

import (
	"cosmossdk.io/math"

	perptypes "github.com/perpdex/perpdex-l1/types"
)

func DefaultGenesis() *GenesisState {
	return &GenesisState{
		Params: DefaultParams(),
		Counters: Counters{
			NextMasterAccountIndex: perptypes.FirstUserMasterAccountIndex,
			NextSubAccountIndex:    perptypes.MinSubAccountIndex,
		},
		Accounts: []Account{
			{
				AccountIndex:       perptypes.TreasuryAccountIndex,
				MasterAccountIndex: perptypes.NilMasterAccountIndex,
				OwnerAddress:       "",
				AccountType:        perptypes.MasterAccountType,
				AccountTradingMode: perptypes.AccountTradingModeSimple,
				Collateral:         math.ZeroInt(),
			},
			{
				AccountIndex:       perptypes.InsuranceFundOperatorAccountIdx,
				MasterAccountIndex: perptypes.NilMasterAccountIndex,
				OwnerAddress:       "",
				AccountType:        perptypes.InsuranceFundAccountType,
				AccountTradingMode: perptypes.AccountTradingModeSimple,
				Collateral:         math.ZeroInt(),
			},
		},
	}
}

func (gs GenesisState) Validate() error {
	if err := gs.Params.Validate(); err != nil {
		return err
	}
	seen := map[uint64]bool{}
	for _, a := range gs.Accounts {
		if seen[a.AccountIndex] {
			return ErrAccountExists.Wrapf("duplicate account_index %d", a.AccountIndex)
		}
		seen[a.AccountIndex] = true
	}
	return nil
}
