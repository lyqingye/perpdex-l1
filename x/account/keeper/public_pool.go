package keeper

import (
	"context"

	"cosmossdk.io/math"

	perptypes "github.com/perpdex/perpdex-l1/types"
	"github.com/perpdex/perpdex-l1/x/account/types"
)

// SharesToUSDCValue implements `get_shares_usdc_value_for_public_pool`:
//
//	if total_shares == 0 ⇒ share * INITIAL_POOL_SHARE_VALUE
//	else                 ⇒ share * |TAV| / (total_shares * USDC_TO_COLLATERAL_MULTIPLIER)
//
// Result is in USDC base units (uusdc), suitable for crediting via
// AddCollateral after multiplying by USDCToCollateralMultiplier.
func (k Keeper) SharesToUSDCValue(
	ctx context.Context,
	poolIdx uint64,
	shareAmount math.Int,
) (math.Int, error) {
	if shareAmount.IsNil() || !shareAmount.IsPositive() {
		return math.ZeroInt(), nil
	}
	pool, err := k.GetAccount(ctx, poolIdx)
	if err != nil {
		return math.ZeroInt(), err
	}
	if pool.PublicPoolInfo == nil {
		return math.ZeroInt(), types.ErrInvalidPoolAccount.Wrapf("account %d", poolIdx)
	}
	info := pool.PublicPoolInfo
	if info.TotalShares.IsZero() {
		return shareAmount.Mul(math.NewIntFromUint64(perptypes.InitialPoolShareValue)), nil
	}
	tav, err := k.riskKeeper.GetTotalAccountValue(ctx, poolIdx)
	if err != nil {
		return math.ZeroInt(), err
	}
	if !tav.IsPositive() {
		// pool is insolvent; LP shares price to zero
		return math.ZeroInt(), nil
	}
	denom := info.TotalShares.Mul(math.NewIntFromUint64(perptypes.USDCToCollateralMultiplier))
	if denom.IsZero() {
		return math.ZeroInt(), nil
	}
	return shareAmount.Mul(tav).Quo(denom), nil
}

// USDCValueToShares is the inverse of SharesToUSDCValue. It computes how
// many shares correspond to a deposit of `usdcAmount` (uusdc) into the
// pool at the current NAV. Used by MintShares.
func (k Keeper) USDCValueToShares(
	ctx context.Context,
	poolIdx uint64,
	usdcAmount math.Int,
) (math.Int, error) {
	if usdcAmount.IsNil() || !usdcAmount.IsPositive() {
		return math.ZeroInt(), nil
	}
	pool, err := k.GetAccount(ctx, poolIdx)
	if err != nil {
		return math.ZeroInt(), err
	}
	if pool.PublicPoolInfo == nil {
		return math.ZeroInt(), types.ErrInvalidPoolAccount.Wrapf("account %d", poolIdx)
	}
	info := pool.PublicPoolInfo
	if info.TotalShares.IsZero() {
		// initial mint: 1 share = INITIAL_POOL_SHARE_VALUE uusdc
		return usdcAmount.Quo(math.NewIntFromUint64(perptypes.InitialPoolShareValue)), nil
	}
	tav, err := k.riskKeeper.GetTotalAccountValue(ctx, poolIdx)
	if err != nil {
		return math.ZeroInt(), err
	}
	if !tav.IsPositive() {
		return math.ZeroInt(), types.ErrPoolNotActive.Wrap("pool TAV is non-positive; mint refused")
	}
	num := usdcAmount.Mul(info.TotalShares).Mul(math.NewIntFromUint64(perptypes.USDCToCollateralMultiplier))
	return num.Quo(tav), nil
}

// AvailableSharesToBurn returns the cap on shares an LP may burn given
// the pool's current available_collateral, implementing
// `get_available_shares_to_burn_for_public_pool`:
//
//	available_shares = available_collateral * total_shares / TAV
func (k Keeper) AvailableSharesToBurn(
	ctx context.Context,
	poolIdx uint64,
) (math.Int, error) {
	pool, err := k.GetAccount(ctx, poolIdx)
	if err != nil {
		return math.ZeroInt(), err
	}
	if pool.PublicPoolInfo == nil {
		return math.ZeroInt(), types.ErrInvalidPoolAccount.Wrapf("account %d", poolIdx)
	}
	info := pool.PublicPoolInfo
	if info.TotalShares.IsZero() {
		return math.ZeroInt(), nil
	}
	tav, err := k.riskKeeper.GetTotalAccountValue(ctx, poolIdx)
	if err != nil {
		return math.ZeroInt(), err
	}
	if !tav.IsPositive() {
		return math.ZeroInt(), nil
	}
	avail, err := k.riskKeeper.GetAvailableCollateral(ctx, poolIdx)
	if err != nil {
		return math.ZeroInt(), err
	}
	if !avail.IsPositive() {
		return math.ZeroInt(), nil
	}
	return avail.Mul(info.TotalShares).Quo(tav), nil
}

// CheckMinOperatorShareRate enforces the post-update invariant
//
//	total_shares * min_rate <= operator_shares * SHARE_TICK
//
// which is the operator's skin-in-the-game floor. Used by Mint
// (non-operator), Burn (operator burn while pool not frozen) and Update.
//
// An empty pool (`total_shares == 0`) trivially satisfies the invariant,
// so the check is skipped.
func CheckMinOperatorShareRate(info types.PublicPoolInfo) bool {
	if info.TotalShares.IsZero() {
		return true
	}
	lhs := info.TotalShares.Mul(math.NewIntFromUint64(uint64(info.MinOperatorShareRate)))
	rhs := info.OperatorShares.Mul(math.NewIntFromUint64(uint64(perptypes.ShareTick)))
	return lhs.LTE(rhs)
}

// EnsureNotFrozen / EnsureActive are kept as thin pass-throughs to the
// canonical types-level guards so existing callers in this package keep
// compiling unchanged. New callers (e.g. liquidation / matching) should
// prefer the types-level helpers directly to avoid pulling the account
// keeper into their import graph.
func EnsureNotFrozen(info *types.PublicPoolInfo) error { return types.EnsureNotFrozen(info) }
func EnsureActive(info *types.PublicPoolInfo) error    { return types.EnsureActive(info) }

// FindShareEntry locates a (user, pool) PublicPoolShare in user.PublicPoolShares.
// Returns the index in the slice + true if present, -1 + false otherwise.
func FindShareEntry(user types.Account, poolIdx uint64) (int, bool) {
	for i := range user.PublicPoolShares {
		if user.PublicPoolShares[i].PublicPoolIndex == poolIdx {
			return i, true
		}
	}
	return -1, false
}

// IsPoolAccount reports whether the account holds a Public Pool /
// Insurance Fund role (i.e. has a PublicPoolInfo and the right type).
// Stronger than IsPoolType: pool-specific invariants (TotalShares /
// Status / NAV) only live on PublicPoolInfo, so callers operating on
// those must require it to be non-nil.
func IsPoolAccount(a types.Account) bool {
	return a.PublicPoolInfo != nil && a.IsPoolType()
}
