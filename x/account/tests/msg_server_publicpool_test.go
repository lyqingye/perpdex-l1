// Msg server defense-in-depth tests for the PublicPool handlers
// (CreatePublicPool / UpdatePublicPool). Each test pins the
// invariant that the handler calls msg.ValidateBasic() on entry so
// stateless guards fire before any keeper state is touched, even
// when the SDK ante is bypassed.
package tests

import (
	"testing"

	"github.com/stretchr/testify/require"

	perptypes "github.com/perpdex/perpdex-l1/types"
	accountkeeper "github.com/perpdex/perpdex-l1/x/account/keeper"
	"github.com/perpdex/perpdex-l1/x/account/types"
)

// TestMsgServer_CreatePublicPool_RejectsZeroShares proves the
// CreatePublicPool handler enforces InitialTotalShares > 0 from
// ValidateBasic at the keeper-call layer.
func TestMsgServer_CreatePublicPool_RejectsZeroShares(t *testing.T) {
	env := initTestEnv(t)
	srv := accountkeeper.NewMsgServerImpl(env.ak)

	_, err := srv.CreatePublicPool(env.ctx, &types.MsgCreatePublicPool{
		Sender:               validOwner,
		MasterAccountIndex:   8200,
		AccountType:          perptypes.PublicPoolAccountType,
		OperatorFee:          0,
		MinOperatorShareRate: 0,
		InitialTotalShares:   0, // ValidateBasic floor.
	})
	require.ErrorIs(t, err, types.ErrInvalidParams)
}

// TestMsgServer_UpdatePublicPool_RejectsInvalidStatus proves the
// UpdatePublicPool handler enforces the NewStatus enum from
// ValidateBasic at the keeper-call layer.
func TestMsgServer_UpdatePublicPool_RejectsInvalidStatus(t *testing.T) {
	env := initTestEnv(t)
	srv := accountkeeper.NewMsgServerImpl(env.ak)

	_, err := srv.UpdatePublicPool(env.ctx, &types.MsgUpdatePublicPool{
		Sender:                  validOwner,
		PoolAccountIndex:        8300,
		NewStatus:               42,
		NewOperatorFee:          0,
		NewMinOperatorShareRate: 0,
	})
	require.ErrorIs(t, err, types.ErrInvalidPoolUpdate)
}
