// Pure types-level genesis validation. These tests target
// types.GenesisState.Validate, which runs before the keeper sees any
// data: duplicate account_index rows, unknown enum values, negative
// collateral, duplicate (account, asset) pairs, and pool accounts
// missing PublicPoolInfo all must be rejected statelessly.
package tests

import (
	"testing"

	"github.com/stretchr/testify/require"

	"cosmossdk.io/math"

	perptypes "github.com/perpdex/perpdex-l1/types"
	"github.com/perpdex/perpdex-l1/x/account/types"
)

// TestGenesisValidate_OK accepts the default genesis.
func TestGenesisValidate_OK(t *testing.T) {
	gs := types.DefaultGenesis()
	require.NoError(t, gs.Validate())
}

// TestGenesisValidate_DuplicateAccount rejects duplicate account_index rows.
func TestGenesisValidate_DuplicateAccount(t *testing.T) {
	gs := types.DefaultGenesis()
	gs.Accounts = append(gs.Accounts, gs.Accounts[0])
	require.ErrorIs(t, gs.Validate(), types.ErrAccountExists)
}

// TestGenesisValidate_InvalidAccountType rejects unknown enum values.
func TestGenesisValidate_InvalidAccountType(t *testing.T) {
	gs := types.DefaultGenesis()
	gs.Accounts[0].AccountType = 99
	require.ErrorIs(t, gs.Validate(), types.ErrInvalidAccountType)
}

// TestGenesisValidate_NegativeCollateral rejects a negative collateral row.
func TestGenesisValidate_NegativeCollateral(t *testing.T) {
	gs := types.DefaultGenesis()
	gs.Accounts[0].Collateral = math.NewInt(-1)
	require.ErrorIs(t, gs.Validate(), types.ErrInvalidParams)
}

// TestGenesisValidate_DuplicateAccountAsset rejects duplicate (account,
// asset) rows.
func TestGenesisValidate_DuplicateAccountAsset(t *testing.T) {
	gs := types.DefaultGenesis()
	row := types.AccountAsset{
		AccountIndex: perptypes.TreasuryAccountIndex,
		AssetIndex:   perptypes.USDCAssetIndex,
		Balance:      math.ZeroInt(),
	}
	gs.AccountAssets = []types.AccountAsset{row, row}
	require.ErrorIs(t, gs.Validate(), types.ErrInvalidParams)
}

// TestGenesisValidate_NegativeAccountAssetBalance rejects a row whose
// balance is negative.
func TestGenesisValidate_NegativeAccountAssetBalance(t *testing.T) {
	gs := types.DefaultGenesis()
	gs.AccountAssets = []types.AccountAsset{{
		AccountIndex: perptypes.TreasuryAccountIndex,
		AssetIndex:   perptypes.USDCAssetIndex,
		Balance:      math.NewInt(-1),
	}}
	require.ErrorIs(t, gs.Validate(), types.ErrInvalidParams)
}

// TestGenesisValidate_PoolMissingInfo rejects a pool account that forgets
// to set PublicPoolInfo.
func TestGenesisValidate_PoolMissingInfo(t *testing.T) {
	gs := types.DefaultGenesis()
	for i := range gs.Accounts {
		if gs.Accounts[i].AccountType == perptypes.InsuranceFundAccountType {
			gs.Accounts[i].PublicPoolInfo = nil
		}
	}
	require.ErrorIs(t, gs.Validate(), types.ErrInvalidPoolAccount)
}
