package types

import (
	"testing"

	"cosmossdk.io/math"

	"github.com/stretchr/testify/require"

	perptypes "github.com/perpdex/perpdex-l1/types"
)

// TestGenesisValidate_OK accepts the default genesis.
func TestGenesisValidate_OK(t *testing.T) {
	gs := DefaultGenesis()
	require.NoError(t, gs.Validate())
}

// TestGenesisValidate_DuplicateAccount rejects duplicate account_index rows.
func TestGenesisValidate_DuplicateAccount(t *testing.T) {
	gs := DefaultGenesis()
	gs.Accounts = append(gs.Accounts, gs.Accounts[0])
	require.ErrorIs(t, gs.Validate(), ErrAccountExists)
}

// TestGenesisValidate_InvalidAccountType rejects unknown enum values.
func TestGenesisValidate_InvalidAccountType(t *testing.T) {
	gs := DefaultGenesis()
	gs.Accounts[0].AccountType = 99
	require.ErrorIs(t, gs.Validate(), ErrInvalidAccountType)
}

// TestGenesisValidate_NegativeCollateral rejects a negative collateral row.
func TestGenesisValidate_NegativeCollateral(t *testing.T) {
	gs := DefaultGenesis()
	gs.Accounts[0].Collateral = math.NewInt(-1)
	require.ErrorIs(t, gs.Validate(), ErrInvalidParams)
}

// TestGenesisValidate_DuplicateAccountAsset rejects duplicate (account,
// asset) rows.
func TestGenesisValidate_DuplicateAccountAsset(t *testing.T) {
	gs := DefaultGenesis()
	row := AccountAsset{
		AccountIndex: perptypes.TreasuryAccountIndex,
		AssetIndex:   perptypes.USDCAssetIndex,
		Balance:      math.ZeroInt(),
	}
	gs.AccountAssets = []AccountAsset{row, row}
	require.ErrorIs(t, gs.Validate(), ErrInvalidParams)
}

// TestGenesisValidate_NegativeAccountAssetBalance rejects a row whose
// balance is negative.
func TestGenesisValidate_NegativeAccountAssetBalance(t *testing.T) {
	gs := DefaultGenesis()
	gs.AccountAssets = []AccountAsset{{
		AccountIndex: perptypes.TreasuryAccountIndex,
		AssetIndex:   perptypes.USDCAssetIndex,
		Balance:      math.NewInt(-1),
	}}
	require.ErrorIs(t, gs.Validate(), ErrInvalidParams)
}

// TestGenesisValidate_PoolMissingInfo rejects a pool account that forgets
// to set PublicPoolInfo.
func TestGenesisValidate_PoolMissingInfo(t *testing.T) {
	gs := DefaultGenesis()
	for i := range gs.Accounts {
		if gs.Accounts[i].AccountType == perptypes.InsuranceFundAccountType {
			gs.Accounts[i].PublicPoolInfo = nil
		}
	}
	require.ErrorIs(t, gs.Validate(), ErrInvalidPoolAccount)
}
