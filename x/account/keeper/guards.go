package keeper

import (
	"context"

	perptypes "github.com/perpdex/perpdex-l1/types"
	"github.com/perpdex/perpdex-l1/x/account/types"
)

// rejectPoolAccount refuses to let the generic account Msg handlers operate on
// public pool / insurance fund accounts. LP collateral must flow exclusively
// through MintShares / BurnShares / StrategyTransfer / liquidation paths so
// the share bookkeeping (TotalShares/OperatorShares/users' PublicPoolShares)
// stays consistent with the pool's NAV.
func (k Keeper) rejectPoolAccount(ctx context.Context, idx uint64) error {
	a, err := k.GetAccount(ctx, idx)
	if err != nil {
		return err
	}
	if a.AccountType == perptypes.PublicPoolAccountType ||
		a.AccountType == perptypes.InsuranceFundAccountType {
		return types.ErrPoolGenericMsg.Wrapf("account %d is a pool account", idx)
	}
	return nil
}

// settleAllPositionFunding settles pending funding for every non-zero perp
// position held by `accountIdx`. Called before Withdraw/Transfer/UpdateMargin
// so the subsequent risk check sees the post-funding EntryQuote and not a
// stale snapshot.
func (k Keeper) settleAllPositionFunding(ctx context.Context, accountIdx uint64) error {
	for marketIdx := uint32(0); marketIdx <= perptypes.MaxPerpsMarketIndex; marketIdx++ {
		pos, err := k.GetPosition(ctx, accountIdx, marketIdx)
		if err != nil {
			return err
		}
		if pos.Position.IsZero() {
			continue
		}
		if err := k.fundingKeeper.SettlePositionFunding(ctx, accountIdx, marketIdx); err != nil {
			return err
		}
	}
	return nil
}

// requireRiskOK enforces a post-state risk check. The risk keeper is wired
// at app construction and is always non-nil at runtime; missing wiring is a
// programming error and will panic, which is the desired fail-fast behaviour.
func (k Keeper) requireRiskOK(ctx context.Context, accountIdx uint64) error {
	ok, err := k.riskKeeper.IsValidRiskChange(ctx, accountIdx)
	if err != nil {
		return err
	}
	if !ok {
		return types.ErrRiskRegression
	}
	return nil
}

// snapshotPreRisk captures the account's pre-state risk so a later
// IsValidRiskChange call can compare deltas instead of demanding a
// strictly-healthy post state.
func (k Keeper) snapshotPreRisk(ctx context.Context, accountIdx uint64) error {
	return k.riskKeeper.SnapshotPreRisk(ctx, accountIdx)
}
