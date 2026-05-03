package app

import "encoding/json"

// GenesisState is the raw genesis state of the application, indexed by
// module name. Its layout matches what the SDK module manager expects.
type GenesisState map[string]json.RawMessage

// NewDefaultGenesisState returns a default genesis state aggregated from
// every module registered in the BasicManager.
func NewDefaultGenesisState(app *PerpDEXApp) GenesisState {
	return app.ModuleBasics.DefaultGenesis(app.appCodec)
}
