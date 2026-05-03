package keepers

import (
	ibctransfertypes "github.com/cosmos/ibc-go/v10/modules/apps/transfer/types"
	ibcexported "github.com/cosmos/ibc-go/v10/modules/core/exported"

	storetypes "cosmossdk.io/store/types"
	evidencetypes "cosmossdk.io/x/evidence/types"
	"cosmossdk.io/x/feegrant"
	upgradetypes "cosmossdk.io/x/upgrade/types"

	authtypes "github.com/cosmos/cosmos-sdk/x/auth/types"
	authzkeeper "github.com/cosmos/cosmos-sdk/x/authz/keeper"
	banktypes "github.com/cosmos/cosmos-sdk/x/bank/types"
	consensusparamtypes "github.com/cosmos/cosmos-sdk/x/consensus/types"
	distrtypes "github.com/cosmos/cosmos-sdk/x/distribution/types"
	govtypes "github.com/cosmos/cosmos-sdk/x/gov/types"
	minttypes "github.com/cosmos/cosmos-sdk/x/mint/types"
	paramstypes "github.com/cosmos/cosmos-sdk/x/params/types"
	slashingtypes "github.com/cosmos/cosmos-sdk/x/slashing/types"
	stakingtypes "github.com/cosmos/cosmos-sdk/x/staking/types"

	accounttypes "github.com/perpdex/perpdex-l1/x/account/types"
	assettypes "github.com/perpdex/perpdex-l1/x/asset/types"
	fundingtypes "github.com/perpdex/perpdex-l1/x/funding/types"
	liquidationtypes "github.com/perpdex/perpdex-l1/x/liquidation/types"
	markettypes "github.com/perpdex/perpdex-l1/x/market/types"
	matchingtypes "github.com/perpdex/perpdex-l1/x/matching/types"
	oracletypes "github.com/perpdex/perpdex-l1/x/oracle/types"
	orderbooktypes "github.com/perpdex/perpdex-l1/x/orderbook/types"
	risktypes "github.com/perpdex/perpdex-l1/x/risk/types"
	tradetypes "github.com/perpdex/perpdex-l1/x/trade/types"
)

// GenerateKeys allocates the KV / transient / memory store keys used by all
// modules registered in the application.
func (appKeepers *AppKeepers) GenerateKeys() {
	appKeepers.keys = storetypes.NewKVStoreKeys(
		authtypes.StoreKey,
		banktypes.StoreKey,
		stakingtypes.StoreKey,
		minttypes.StoreKey,
		distrtypes.StoreKey,
		slashingtypes.StoreKey,
		govtypes.StoreKey,
		paramstypes.StoreKey,
		consensusparamtypes.StoreKey,
		upgradetypes.StoreKey,
		evidencetypes.StoreKey,
		feegrant.StoreKey,
		authzkeeper.StoreKey,
		ibcexported.StoreKey,
		ibctransfertypes.StoreKey,

		// Perpdex modules.
		assettypes.StoreKey,
		accounttypes.StoreKey,
		markettypes.StoreKey,
		oracletypes.StoreKey,
		orderbooktypes.StoreKey,
		fundingtypes.StoreKey,
		risktypes.StoreKey,
		tradetypes.StoreKey,
		matchingtypes.StoreKey,
		liquidationtypes.StoreKey,
	)

	appKeepers.tkeys = storetypes.NewTransientStoreKeys(paramstypes.TStoreKey)
	appKeepers.memKeys = storetypes.NewMemoryStoreKeys()
}

func (appKeepers *AppKeepers) GetKVStoreKey() map[string]*storetypes.KVStoreKey {
	return appKeepers.keys
}

func (appKeepers *AppKeepers) GetTransientStoreKey() map[string]*storetypes.TransientStoreKey {
	return appKeepers.tkeys
}

func (appKeepers *AppKeepers) GetMemoryStoreKey() map[string]*storetypes.MemoryStoreKey {
	return appKeepers.memKeys
}

// GetKey returns the KVStoreKey for the provided store key.
func (appKeepers *AppKeepers) GetKey(storeKey string) *storetypes.KVStoreKey {
	return appKeepers.keys[storeKey]
}

// GetTKey returns the TransientStoreKey for the provided store key.
func (appKeepers *AppKeepers) GetTKey(storeKey string) *storetypes.TransientStoreKey {
	return appKeepers.tkeys[storeKey]
}

// GetMemKey returns the MemoryStoreKey for the provided store key.
func (appKeepers *AppKeepers) GetMemKey(storeKey string) *storetypes.MemoryStoreKey {
	return appKeepers.memKeys[storeKey]
}
