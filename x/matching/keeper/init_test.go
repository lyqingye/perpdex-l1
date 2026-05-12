package keeper

import (
	"os"
	"testing"

	sdk "github.com/cosmos/cosmos-sdk/types"

	perptypes "github.com/perpdex/perpdex-l1/types"
)

// TestMain configures the chain-wide bech32 prefixes (sourced from
// types/constants.go so tests stay in lockstep with the runtime
// `app.SetAddressPrefixes` wiring) once per process. msg_server
// handlers now defensively re-run `msg.ValidateBasic()` since SDK
// v0.53 ante no longer iterates Msgs for stateless validation, which
// means every keeper test that synthesises bech32 sender addresses
// must boot with the right prefix.
func TestMain(m *testing.M) {
	cfg := sdk.GetConfig()
	cfg.SetBech32PrefixForAccount(perptypes.Bech32PrefixAccAddr, perptypes.Bech32PrefixAccPub)
	cfg.SetBech32PrefixForValidator(perptypes.Bech32PrefixValAddr, perptypes.Bech32PrefixValPub)
	cfg.SetBech32PrefixForConsensusNode(perptypes.Bech32PrefixConsAddr, perptypes.Bech32PrefixConsPub)
	os.Exit(m.Run())
}
