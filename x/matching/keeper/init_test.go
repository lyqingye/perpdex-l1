package keeper

import (
	"os"
	"testing"

	sdk "github.com/cosmos/cosmos-sdk/types"
)

// TestMain configures the chain-wide `px` bech32 prefix once per
// process so msg_server handlers (which now defensively re-run
// `msg.ValidateBasic()` since SDK v0.53 ante no longer iterates Msgs
// for stateless validation) accept the canonical test addresses used
// throughout the keeper tests.
func TestMain(m *testing.M) {
	cfg := sdk.GetConfig()
	cfg.SetBech32PrefixForAccount("px", "pxpub")
	cfg.SetBech32PrefixForValidator("pxvaloper", "pxvaloperpub")
	cfg.SetBech32PrefixForConsensusNode("pxvalcons", "pxvalconspub")
	os.Exit(m.Run())
}
