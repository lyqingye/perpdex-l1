// Package tests holds the unified test suite for x/account. The
// TestMain hook installs the chain-wide `px` bech32 prefixes once per
// process so any test that goes through sdk.MustAccAddressFromBech32
// (or that runs ValidateBasic on a sender field) sees the canonical
// addressing the production chain uses.
package tests

import (
	"os"
	"testing"

	sdk "github.com/cosmos/cosmos-sdk/types"
)

func TestMain(m *testing.M) {
	cfg := sdk.GetConfig()
	cfg.SetBech32PrefixForAccount("px", "pxpub")
	cfg.SetBech32PrefixForValidator("pxvaloper", "pxvaloperpub")
	cfg.SetBech32PrefixForConsensusNode("pxvalcons", "pxvalconspub")
	os.Exit(m.Run())
}
