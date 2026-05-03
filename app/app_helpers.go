package app

import (
	ibckeeper "github.com/cosmos/ibc-go/v10/modules/core/keeper"
)

// GetIBCKeeper implements the ibctesting.TestingApp interface.
func (app *PerpDEXApp) GetIBCKeeper() *ibckeeper.Keeper { //nolint:nolintlint
	return app.IBCKeeper
}
