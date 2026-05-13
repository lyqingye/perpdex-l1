package keeper

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"

	perptypes "github.com/perpdex/perpdex-l1/types"
	markettypes "github.com/perpdex/perpdex-l1/x/market/types"
	"github.com/perpdex/perpdex-l1/x/matching/types"
)

// stubPreLiqRisk is a minimal RiskKeeper used to drive checkPreLiquidationGate.
type stubPreLiqRisk struct {
	cross uint32
	iso   uint32
}

func (s stubPreLiqRisk) GetHealthStatus(_ context.Context, _ uint64) (uint32, error) {
	return s.cross, nil
}
func (s stubPreLiqRisk) GetIsolatedHealthStatus(_ context.Context, _ uint64, _ uint32) (uint32, error) {
	return s.iso, nil
}

// GetMarkAndMarketDetails is unused by checkPreLiquidationGate; the
// stub returns a benign fresh mark so the matching RiskKeeper
// interface is satisfied.
func (stubPreLiqRisk) GetMarkAndMarketDetails(_ context.Context, mkt uint32) (uint32, markettypes.MarketDetails, error) {
	return 1, markettypes.MarketDetails{MarketIndex: mkt, MarkPrice: 1, LastMarkPriceTimestamp: 1}, nil
}

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
			m := msgServer{
				Keeper: Keeper{
					riskKeeper: stubPreLiqRisk{cross: c.cross, iso: c.iso},
				},
			}
			err := m.checkPreLiquidationGate(context.Background(), 1, 0, c.reduceOnly)
			if c.wantErr {
				require.Error(t, err)
				require.ErrorIs(t, err, types.ErrAccountUnderLiquidation)
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
	m := msgServer{Keeper: Keeper{}}
	require.NoError(t, m.checkPreLiquidationGate(context.Background(), 1, 0, false))
}
