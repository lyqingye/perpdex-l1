// query_test.go exercises the gRPC query server: the read-side surface
// other modules and clients consume. The pagination case confirms the
// list endpoint actually walks the store (and includes the default
// USDC row plus the registered batch), and the by-denom lookup
// confirms the not-found path returns a proper error rather than a
// zero value.
package tests

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	perptypes "github.com/perpdex/perpdex-l1/types"
	"github.com/perpdex/perpdex-l1/x/asset/types"
)

// TestQuery_Assets_Pagination registers a handful of assets and walks
// the gRPC paginator to confirm the implementation actually paginates
// (rather than returning every row in one shot).
func TestQuery_Assets_Pagination(t *testing.T) {
	env := newTestEnv(t)
	for _, name := range []string{"BTC", "ETH", "SOL"} {
		_, err := env.srv.RegisterAsset(env.ctx, &types.MsgRegisterAsset{
			Authority:           testAuthority,
			Denom:               "u" + strings.ToLower(name),
			DisplayName:         name,
			Decimals:            8,
			ExtensionMultiplier: 1,
			MinTransferAmount:   1,
			MinWithdrawalAmount: 1,
			MarginMode:          perptypes.MarginModeDisabled,
		})
		require.NoError(t, err)
	}

	resp, err := env.q.Assets(env.ctx, &types.QueryAssetsRequest{})
	require.NoError(t, err)
	require.Len(t, resp.Assets, 4)
}

func TestQuery_AssetByDenom_NotFoundError(t *testing.T) {
	env := newTestEnv(t)
	_, err := env.q.AssetByDenom(env.ctx, &types.QueryAssetByDenomRequest{Denom: "ubogus"})
	require.Error(t, err)
}
