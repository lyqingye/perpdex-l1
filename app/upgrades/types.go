package upgrades

import (
	store "cosmossdk.io/store/types"
	upgradetypes "cosmossdk.io/x/upgrade/types"

	"github.com/cosmos/cosmos-sdk/types/module"

	"github.com/perpdex/perpdex-l1/app/keepers"
)

// Upgrade describes a single in-place store migration. Add upgrades to
// `app.Upgrades` in app.go to wire them into the upgrade keeper.
type Upgrade struct {
	UpgradeName          string
	CreateUpgradeHandler func(*module.Manager, module.Configurator, *keepers.AppKeepers) upgradetypes.UpgradeHandler
	StoreUpgrades        store.StoreUpgrades
}
