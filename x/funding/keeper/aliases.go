package keeper

import (
	"github.com/perpdex/perpdex-l1/x/funding/types"
)

// internal/keeper relies on the Params struct as a value type. We re-export it
// as a local alias so the abci.go file compiles cleanly even though
// types.Params is generated.
type _ = types.Params
