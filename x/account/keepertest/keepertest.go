// Package keepertest provides write helpers for fixture setup in
// tests. Production code MUST NOT import this package — it bypasses
// the cohesive mutator API on x/account.Keeper
// (UpdateAccountTradingMode, UpdatePublicPoolInfo, ApplyFill /
// AdjustAllocatedMargin / ApplyFundingPayment / SetPositionLeverage /
// ClosePosition, TransferAccountAssetBalance, etc.) and writes the
// underlying collections directly. The helpers only exist so existing
// keeper / e2e suites can keep their table-driven fixture style
// without re-implementing the storage schema.
//
// New tests should prefer the cohesive mutator API or scenario-level
// flows (Deposit, MintShares, etc.) for state setup; reach for these
// helpers only when a test needs to construct a state that the
// mutator API can't produce by design (e.g. injecting a malformed
// AccountAsset row to verify error handling).
package keepertest

import (
	"context"

	"cosmossdk.io/collections"

	perptypes "github.com/perpdex/perpdex-l1/types"
	accountkeeper "github.com/perpdex/perpdex-l1/x/account/keeper"
	"github.com/perpdex/perpdex-l1/x/account/types"
)

// SetAccountForTest writes `a` directly to the Accounts collection
// and (when applicable) maintains the OwnerToIndex pointer +
// MasterSubAccounts index. Test-only helper: the production code path
// goes through CreateAccount / dedicated keeper APIs.
func SetAccountForTest(ctx context.Context, k accountkeeper.Keeper, a types.Account) error {
	if err := k.Accounts.Set(ctx, a.AccountIndex, a); err != nil {
		return err
	}
	if a.OwnerAddress != "" && a.AccountType == perptypes.MasterAccountType {
		if err := k.OwnerToIndex.Set(ctx, a.OwnerAddress, a.AccountIndex); err != nil {
			return err
		}
	}
	if a.AccountType != perptypes.MasterAccountType && a.MasterAccountIndex != perptypes.NilMasterAccountIndex {
		if err := k.MasterSubAccounts.Set(ctx, collections.Join(a.MasterAccountIndex, a.AccountIndex)); err != nil {
			return err
		}
	}
	return nil
}

// SetPositionForTest writes `p` directly to the AccountPositions
// collection.
func SetPositionForTest(ctx context.Context, k accountkeeper.Keeper, p types.AccountPosition) error {
	return k.AccountPositions.Set(ctx, collections.Join(p.AccountIndex, p.MarketIndex), p)
}

// SetAccountAssetForTest writes `aa` directly to the AccountAssets
// collection.
func SetAccountAssetForTest(ctx context.Context, k accountkeeper.Keeper, aa types.AccountAsset) error {
	return k.AccountAssets.Set(ctx, collections.Join(aa.AccountIndex, aa.AssetIndex), aa)
}
