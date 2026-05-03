// Package common holds helpers shared between the e2e suite and per-scenario
// test files that do not depend on a running suite. Anything that needs to
// touch *perp.PerpDEXApp / sdk.Context lives in the sibling `msg` and
// `query` packages; this one is for pure-Go utilities (key generation,
// math helpers, default constants).
package common

import (
	"github.com/cosmos/cosmos-sdk/crypto/keys/secp256k1"
	cryptotypes "github.com/cosmos/cosmos-sdk/crypto/types"
	sdk "github.com/cosmos/cosmos-sdk/types"
)

// TestUser is the data carrier used by both the root suite and the helper
// packages. Re-exported here to avoid import cycles: helper packages can
// take a `common.TestUser` without depending back on `tests/e2e`.
type TestUser struct {
	PrivKey      cryptotypes.PrivKey
	Address      sdk.AccAddress
	AccountIndex uint64
}

// NewUser allocates a new random secp256k1 wallet for tests.
func NewUser() TestUser {
	pk := secp256k1.GenPrivKey()
	return TestUser{PrivKey: pk, Address: sdk.AccAddress(pk.PubKey().Address())}
}

// NewUsers returns n freshly-generated TestUsers.
func NewUsers(n int) []TestUser {
	users := make([]TestUser, n)
	for i := range users {
		users[i] = NewUser()
	}
	return users
}
