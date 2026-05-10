package keeper

import (
	"context"

	"cosmossdk.io/math"

	risktypes "github.com/perpdex/perpdex-l1/x/risk/types"
)

// RiskKeeper is consulted to ensure each state change leaves the
// account healthy or strictly improving.
//
// The interface lives in x/account/keeper rather than x/account/types
// because its method signatures reference x/risk/types.PreRiskSnapshot,
// and x/risk/types already imports x/account/types (for
// AccountPosition in LiquidationRiskSnapshot). Putting the interface
// next to the consumer keeper avoids the cycle and matches the Go
// idiom of declaring interfaces at the consumption site.
type RiskKeeper interface {
	GetAvailableCollateral(ctx context.Context, accountIndex uint64) (math.Int, error)
	// GetTotalAccountValue returns TAV (collateral + signed
	// unrealized PnL) across every market. Used for share NAV
	// calculations.
	GetTotalAccountValue(ctx context.Context, accountIndex uint64) (math.Int, error)
	// GetHealthStatus mirrors x/risk health classification used by
	// the freeze invariants (freeze requires HEALTHY).
	GetHealthStatus(ctx context.Context, accountIndex uint64) (uint32, error)
	// SnapshotRisk computes the pre-state risk envelope for an
	// account and returns it by value. The caller threads the
	// returned snapshot into IsValidRiskChangeFrom after performing
	// the state mutation; the keeper does not persist pre-state
	// across handlers.
	SnapshotRisk(ctx context.Context, accountIndex uint64) (risktypes.PreRiskSnapshot, error)
	// IsValidRiskChangeFrom enforces the post-state vs pre-state
	// risk invariants. `pre` MUST be the value returned by
	// SnapshotRisk at the start of the same handler.
	IsValidRiskChangeFrom(ctx context.Context, accountIndex uint64, pre risktypes.PreRiskSnapshot) (bool, error)
}
