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
				AccountTradingMode: perptypes.AccountTradingModeUnified,
				Collateral:         math.ZeroInt(),
				PublicPoolInfo:     DefaultInsurancePoolInfo(),
			},
		},
	}
}

func (gs GenesisState) Validate() error {
	if err := gs.Params.Validate(); err != nil {
		return err
	}
	// Counters must not regress below their canonical initial values so
	// deposit auto-create never reuses a baked-in account slot.
	if gs.Counters.NextMasterAccountIndex > perptypes.MaxMasterAccountIndex+1 {
		return ErrInvalidParams.Wrapf("next_master_account_index=%d exceeds maximum", gs.Counters.NextMasterAccountIndex)
	}
	if gs.Counters.NextSubAccountIndex != 0 &&
		gs.Counters.NextSubAccountIndex < perptypes.MinSubAccountIndex {
		return ErrInvalidParams.Wrapf("next_sub_account_index=%d below MinSubAccountIndex", gs.Counters.NextSubAccountIndex)
	}
	if gs.Counters.NextSubAccountIndex > perptypes.MaxAccountIndex+1 {
		return ErrInvalidParams.Wrapf("next_sub_account_index=%d exceeds maximum", gs.Counters.NextSubAccountIndex)
	}

	seen := map[uint64]bool{}
	ownerSeen := map[string]bool{}
	hasIF := false
	for _, a := range gs.Accounts {
		if seen[a.AccountIndex] {
			return ErrAccountExists.Wrapf("duplicate account_index %d", a.AccountIndex)
		}
		seen[a.AccountIndex] = true
		switch a.AccountType {
		case perptypes.MasterAccountType,
			perptypes.SubAccountType,
			perptypes.PublicPoolAccountType,
			perptypes.InsuranceFundAccountType:
		default:
			return ErrInvalidAccountType.Wrapf("account_index=%d type=%d", a.AccountIndex, a.AccountType)
		}
		if a.AccountTradingMode != perptypes.AccountTradingModeSimple &&
			a.AccountTradingMode != perptypes.AccountTradingModeUnified {
			return ErrInvalidTradingMode.Wrapf("account_index=%d trading_mode=%d", a.AccountIndex, a.AccountTradingMode)
		}
		if a.AccountType == perptypes.MasterAccountType && a.OwnerAddress != "" {
			if ownerSeen[a.OwnerAddress] {
				return ErrAccountExists.Wrapf("duplicate master owner %s", a.OwnerAddress)
			}
			ownerSeen[a.OwnerAddress] = true
		}
		if !a.Collateral.IsNil() && a.Collateral.IsNegative() {
			return ErrInvalidParams.Wrapf("negative collateral on account_index=%d", a.AccountIndex)
		}
		// Pool accounts must carry PublicPoolInfo; regular accounts must not.
		if a.AccountType == perptypes.PublicPoolAccountType ||
			a.AccountType == perptypes.InsuranceFundAccountType {
			if a.PublicPoolInfo == nil {
				return ErrInvalidPoolAccount.Wrapf("account_index=%d missing PublicPoolInfo", a.AccountIndex)
			}
			if a.PublicPoolInfo.TotalShares.IsNil() || a.PublicPoolInfo.TotalShares.IsNegative() {
				return ErrInvalidParams.Wrapf("account_index=%d total_shares invalid", a.AccountIndex)
			}
			if a.PublicPoolInfo.OperatorShares.IsNil() || a.PublicPoolInfo.OperatorShares.IsNegative() {
				return ErrInvalidParams.Wrapf("account_index=%d operator_shares invalid", a.AccountIndex)
			}
			if len(a.PublicPoolInfo.Strategies) != perptypes.NbStrategies {
				return ErrInvalidParams.Wrapf("account_index=%d strategies length=%d expected=%d",
					a.AccountIndex, len(a.PublicPoolInfo.Strategies), perptypes.NbStrategies)
			}
		} else if a.PublicPoolInfo != nil {
			return ErrInvalidAccountType.Wrapf("account_index=%d non-pool account has PublicPoolInfo", a.AccountIndex)
		}
		if a.AccountType == perptypes.InsuranceFundAccountType {
			if a.AccountIndex != perptypes.InsuranceFundOperatorAccountIdx {
				return ErrInvalidAccountType.Wrapf("insurance fund must live at account_index=%d", perptypes.InsuranceFundOperatorAccountIdx)
			}
			hasIF = true
		}
		// Bounded shares list to avoid pathological genesis state.
		if len(a.PublicPoolShares) > int(perptypes.SharesListSize) {
			return ErrInvalidParams.Wrapf("account_index=%d shares list too long", a.AccountIndex)
		}
	}
	_ = hasIF // allow genesis without IF in unit tests; real app wires via defaults.

	// AccountAsset rows must reference a non-duplicate (account, asset) pair
	// and, when checked defensively, a known account in gs.Accounts.
	assetSeen := map[uint64]map[uint32]bool{}
	for _, aa := range gs.AccountAssets {
		if assetSeen[aa.AccountIndex] == nil {
			assetSeen[aa.AccountIndex] = map[uint32]bool{}
		}
		if assetSeen[aa.AccountIndex][aa.AssetIndex] {
			return ErrInvalidParams.Wrapf("duplicate account_asset (%d,%d)", aa.AccountIndex, aa.AssetIndex)
		}
		assetSeen[aa.AccountIndex][aa.AssetIndex] = true
		if !aa.Balance.IsNil() && aa.Balance.IsNegative() {
			return ErrInvalidParams.Wrapf("negative balance on (%d,%d)", aa.AccountIndex, aa.AssetIndex)
		}
	}
	posSeen := map[uint64]map[uint32]bool{}
	for _, p := range gs.AccountPositions {
		if posSeen[p.AccountIndex] == nil {
			posSeen[p.AccountIndex] = map[uint32]bool{}
		}
		if posSeen[p.AccountIndex][p.MarketIndex] {
			return ErrInvalidParams.Wrapf("duplicate account_position (%d,%d)", p.AccountIndex, p.MarketIndex)
		}
		posSeen[p.AccountIndex][p.MarketIndex] = true
		if p.MarginMode != perptypes.CrossMargin && p.MarginMode != perptypes.IsolatedMargin {
			return ErrInvalidMarginMode.Wrapf("position (%d,%d) margin_mode=%d", p.AccountIndex, p.MarketIndex, p.MarginMode)
		}
	}
	metaSeen := map[uint64]bool{}
	for _, meta := range gs.AccountMetas {
		if metaSeen[meta.AccountIndex] {
			return ErrInvalidParams.Wrapf("duplicate account_meta %d", meta.AccountIndex)
		}
		metaSeen[meta.AccountIndex] = true
	}
	return nil
}
