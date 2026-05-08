package types

import (
	"cosmossdk.io/math"

	perptypes "github.com/perpdex/perpdex-l1/types"
)

// NormalizeIntFields rewrites every nil math.Int on the account (and on
// its embedded PublicPoolInfo, when present) to math.ZeroInt().
func (a *Account) NormalizeIntFields() {
	if a.Collateral.IsNil() {
		a.Collateral = math.ZeroInt()
	}
	if a.PublicPoolInfo != nil {
		if a.PublicPoolInfo.TotalShares.IsNil() {
			a.PublicPoolInfo.TotalShares = math.ZeroInt()
		}
		if a.PublicPoolInfo.OperatorShares.IsNil() {
			a.PublicPoolInfo.OperatorShares = math.ZeroInt()
		}
		for i, s := range a.PublicPoolInfo.Strategies {
			if s.IsNil() {
				a.PublicPoolInfo.Strategies[i] = math.ZeroInt()
			}
		}
	}
}

// NormalizeIntFields rewrites every nil math.Int on the spot-asset row
// to math.ZeroInt().
func (a *AccountAsset) NormalizeIntFields() {
	if a.Balance.IsNil() {
		a.Balance = math.ZeroInt()
	}
	if a.LockedBalance.IsNil() {
		a.LockedBalance = math.ZeroInt()
	}
}

// IsPoolType reports whether the account's `AccountType` is one of the
// public-pool / insurance-fund roles. This is the lightweight type-only
// check used by routing / Msg-guards (e.g. "pool accounts cannot place
// generic orders", "skip pool accounts when iterating ADL candidates").
//
// Distinct from keeper.IsPoolAccount, which additionally requires
// `PublicPoolInfo != nil`. Callers that operate on pool-specific
// invariants (status, total_shares, …) should keep using IsPoolAccount;
// callers that just need to gate by type-bit use this helper.
func (a Account) IsPoolType() bool {
	return a.AccountType == perptypes.PublicPoolAccountType ||
		a.AccountType == perptypes.InsuranceFundAccountType
}

// EnsureNotFrozen rejects state transitions on a frozen public pool.
// Pure value-level guard so it can live in `types`; keeper packages
// re-export it via accountkeeper.EnsureNotFrozen for legacy callers.
func EnsureNotFrozen(info *PublicPoolInfo) error {
	if info == nil {
		return ErrInvalidPoolAccount
	}
	if info.Status == perptypes.PublicPoolStatusFrozen {
		return ErrPoolFrozen
	}
	return nil
}

// EnsureActive rejects state transitions when the pool is anything other
// than ACTIVE (FROZEN / WIND_DOWN both fail). Use for the LLP / IF /
// deleverager-pool gating in liquidation / ADL / publicpool Msg paths.
func EnsureActive(info *PublicPoolInfo) error {
	if info == nil {
		return ErrInvalidPoolAccount
	}
	if info.Status != perptypes.PublicPoolStatusActive {
		return ErrPoolNotActive.Wrapf("status=%d", info.Status)
	}
	return nil
}
