// preliq_gate_test.go covers the order-placement health gate that
// keeper.(MsgServer).CheckPreLiquidationGate applies before any
// orderbook mutation (CreateOrder / ModifyOrder). The truth table
// follows the spec: HEALTHY allows everything, PRE_LIQUIDATION only
// allows reduce-only, anything deeper rejects every user-initiated
// order. The gate consults BOTH cross-account health and the
// per-market isolated health; either being unhealthy is enough to
// reject.
//
// A separate degraded-mode test pins the "risk keeper unwired"
// fallback (`riskKeeper == nil`) — the gate must no-op so tests /
// staged genesis that omit risk wiring keep working.
package tests

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"

	perptypes "github.com/perpdex/perpdex-l1/types"
	matchingkeeper "github.com/perpdex/perpdex-l1/x/matching/keeper"
	matchingtypes "github.com/perpdex/perpdex-l1/x/matching/types"
)

// TestCheckPreLiquidationGate exercises the matching-side pre-liquidation
// order-placement rule against the full spec table:
//
//	cross / iso health     reduce-only?   expected
//	HEALTHY / HEALTHY      either         allow
//	PRE     / HEALTHY      no             reject
//	PRE     / HEALTHY      yes            allow
//	HEALTHY / PRE          no             reject
//	PARTIAL / HEALTHY      either         reject
//	HEALTHY / FULL         either         reject
//	HEALTHY / BANKRUPTCY   either         reject
func TestCheckPreLiquidationGate(t *testing.T) {
	cases := []struct {
		name       string
		cross      uint32
		iso        uint32
		reduceOnly bool
		wantErr    bool
	}{
		{"healthy_anything_ok", perptypes.HealthHealthy, perptypes.HealthHealthy, false, false},
		{"pre_cross_blocks_increase", perptypes.HealthPreLiquidation, perptypes.HealthHealthy, false, true},
		{"pre_cross_allows_reduce_only", perptypes.HealthPreLiquidation, perptypes.HealthHealthy, true, false},
		{"pre_iso_blocks_increase", perptypes.HealthHealthy, perptypes.HealthPreLiquidation, false, true},
		{"pre_iso_allows_reduce_only", perptypes.HealthHealthy, perptypes.HealthPreLiquidation, true, false},
		{"partial_blocks_all", perptypes.HealthPartialLiquidation, perptypes.HealthHealthy, true, true},
		{"full_blocks_all", perptypes.HealthHealthy, perptypes.HealthFullLiquidation, true, true},
		{"bankruptcy_blocks_all", perptypes.HealthBankruptcy, perptypes.HealthBankruptcy, true, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			k := matchingkeeper.Keeper{}
			k.SetRiskKeeper(stubPreLiqRisk{cross: c.cross, iso: c.iso})
			m := matchingkeeper.MsgServer{Keeper: k}
			err := m.CheckPreLiquidationGate(context.Background(), 1, 0, c.reduceOnly)
			if c.wantErr {
				require.Error(t, err)
				require.ErrorIs(t, err, matchingtypes.ErrAccountUnderLiquidation)
			} else {
				require.NoError(t, err)
			}
		})
	}
}

// TestCheckPreLiquidationGate_NilRiskNoOp verifies the gate degrades to a
// no-op when the risk keeper is unwired (some tests construct the
// matching keeper without late-binding risk).
func TestCheckPreLiquidationGate_NilRiskNoOp(t *testing.T) {
	m := matchingkeeper.MsgServer{Keeper: matchingkeeper.Keeper{}}
	require.NoError(t, m.CheckPreLiquidationGate(context.Background(), 1, 0, false))
}
