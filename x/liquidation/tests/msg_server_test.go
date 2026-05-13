// MsgServer entry-point authorization for x/liquidation.
//
// Covers sender-ownership checks at the gRPC boundary: a Deleverage
// caller must own the deleverager account index they pass, otherwise
// the request is rejected with ErrUnauthorized before the keeper
// flow runs.
package tests

import (
	"testing"

	"cosmossdk.io/math"

	"github.com/stretchr/testify/require"

	perptypes "github.com/perpdex/perpdex-l1/types"
	accounttypes "github.com/perpdex/perpdex-l1/x/account/types"
	liqkeeper "github.com/perpdex/perpdex-l1/x/liquidation/keeper"
	liqtypes "github.com/perpdex/perpdex-l1/x/liquidation/types"
)

// TestDeleverage_RejectsUnauthorizedUserSender verifies that an ADL sender
// can only drive a deleverager account they own (audit msg server fix).
func TestDeleverage_RejectsUnauthorizedUserSender(t *testing.T) {
	ak := newStubAccount()
	authority := "px1qv9pzxqlyckngw6zf9g9whn9d3eh4qvgsxc8cx"
	owner := "px1zqt3uffvxvayzjz02ewkg6mj0xqg0r5463hcq9"
	outsider := "px1r5jzkv3egpr5u42uvd48z7rls6xefxazenrll9"

	ak.accounts[50] = accounttypes.Account{
		AccountIndex: 50, OwnerAddress: owner,
		AccountType: perptypes.MasterAccountType, Collateral: math.ZeroInt(),
	}
	rk := newStubRisk()
	rk.status = perptypes.HealthFullLiquidation
	tk := &stubTrade{}
	matchk := newStubMatching()
	k, ctx := newKeeper(t, ak, rk, tk, matchk)
	srv := liqkeeper.NewMsgServerImpl(k)

	_, err := srv.Deleverage(ctx, &liqtypes.MsgDeleverage{
		Sender:                  outsider,
		VictimAccountIndex:      10,
		MarketIndex:             0,
		DeleveragerAccountIndex: 50,
		BaseAmount:              1,
	})
	require.ErrorIs(t, err, liqtypes.ErrUnauthorized)
	_ = authority
}
