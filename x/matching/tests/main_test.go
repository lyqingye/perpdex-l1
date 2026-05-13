// Package tests hosts the external (`package tests`) test surface for
// x/matching. The matching tests live outside `package keeper` so the
// production code is exercised only through its public Go API; any
// previously package-private hook the tests still need is exported by
// keeper (see `keeper.MatchOrder`, `keeper.MsgServer`,
// `keeper.QuoteExceedsLimit`, `keeper.IsTriggerOrder`,
// `keeper.(*Keeper).SetTradeKeeper`, etc.).
//
// This file owns the per-process `TestMain` that wires the chain-wide
// bech32 prefixes from `types/constants.go` so every keeper test that
// synthesises bech32 sender addresses (CreateOrder / CancelAllOrders /
// ModifyOrder) sees the right prefix. SDK v0.53 ante no longer iterates
// Msgs for stateless validation, so `msg_server` re-runs
// `msg.ValidateBasic()` defensively and demands the prefix is set.
package tests

import (
	"os"
	"testing"

	sdk "github.com/cosmos/cosmos-sdk/types"

	perptypes "github.com/perpdex/perpdex-l1/types"
)

func TestMain(m *testing.M) {
	cfg := sdk.GetConfig()
	cfg.SetBech32PrefixForAccount(perptypes.Bech32PrefixAccAddr, perptypes.Bech32PrefixAccPub)
	cfg.SetBech32PrefixForValidator(perptypes.Bech32PrefixValAddr, perptypes.Bech32PrefixValPub)
	cfg.SetBech32PrefixForConsensusNode(perptypes.Bech32PrefixConsAddr, perptypes.Bech32PrefixConsPub)
	os.Exit(m.Run())
}
