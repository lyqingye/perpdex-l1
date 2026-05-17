package keeper

import (
	"context"

	"cosmossdk.io/math"

	sdk "github.com/cosmos/cosmos-sdk/types"

	perptypes "github.com/perpdex/perpdex-l1/types"
	"github.com/perpdex/perpdex-l1/x/account/types"
)

// PublicPoolAccountParams carries the inputs needed to mint a fresh
// PUBLIC_POOL / INSURANCE_FUND sub-account. Callers fill the master
// reference and the seed PublicPoolInfo; the keeper handles index
// allocation and timestamping.
type PublicPoolAccountParams struct {
	Master             types.Account
	AccountType        uint32
	AccountTradingMode uint32
	SeedCollateral     math.Int
	Info               *types.PublicPoolInfo
}

// CreatePublicPoolAccount allocates the next sub-account index and
// persists a brand-new pool account in a single step. Used by
// Msg.CreatePublicPool.
func (k Keeper) CreatePublicPoolAccount(
	ctx context.Context,
	p PublicPoolAccountParams,
) (types.Account, error) {
	idx, err := k.allocatePoolSubAccountIndex(ctx)
	if err != nil {
		return types.Account{}, err
	}
	now := sdk.UnwrapSDKContext(ctx).BlockTime().UnixMilli()
	pool := types.Account{
		AccountIndex:       idx,
		MasterAccountIndex: p.Master.AccountIndex,
		OwnerAddress:       p.Master.OwnerAddress,
		AccountType:        p.AccountType,
		AccountTradingMode: p.AccountTradingMode,
		Collateral:         p.SeedCollateral,
		CreatedAt:          now,
		PublicPoolInfo:     p.Info,
	}
	if err := k.createAccount(ctx, pool); err != nil {
		return types.Account{}, err
	}
	return pool, nil
}

// allocatePoolSubAccountIndex pulls the next sub-account index,
// skipping any reserved range. Mirrors CreateSubAccount's allocation
// without the master-type guard so the IF master (nil owner) can
// also spawn sub-accounts. Internal helper used by
// CreatePublicPoolAccount.
func (k Keeper) allocatePoolSubAccountIndex(ctx context.Context) (uint64, error) {
	idx, err := k.NextSubIndex.Next(ctx)
	if err != nil {
		return 0, err
	}
	if idx < perptypes.MinSubAccountIndex {
		idx = perptypes.MinSubAccountIndex
		if err := k.NextSubIndex.Set(ctx, idx+1); err != nil {
			return 0, err
		}
	}
	if idx > perptypes.MaxAccountIndex {
		return 0, types.ErrAccountIndexExceed.Wrapf("sub idx=%d", idx)
	}
	return idx, nil
}

// UpdatePublicPoolInfo loads the pool account, runs `mut` against
// its `PublicPoolInfo` pointer, and persists. Returns the updated
// account. If the loaded account is not a pool / has no
// PublicPoolInfo the caller gets `ErrInvalidPoolAccount`. If `mut`
// returns an error the account is NOT persisted.
//
// Single entry point for the GetAccount → mutate info → SetAccount
// pattern, shared by UpdatePublicPool / MintShares / BurnShares /
// StrategyTransfer.
func (k Keeper) UpdatePublicPoolInfo(
	ctx context.Context,
	idx uint64,
	mut func(*types.PublicPoolInfo) error,
) (types.Account, error) {
	a, err := k.GetAccount(ctx, idx)
	if err != nil {
		return types.Account{}, err
	}
	if a.PublicPoolInfo == nil {
		return types.Account{}, types.ErrInvalidPoolAccount.Wrapf("account %d", idx)
	}
	if err := mut(a.PublicPoolInfo); err != nil {
		return types.Account{}, err
	}
	if err := k.updateAccount(ctx, a); err != nil {
		return types.Account{}, err
	}
	return a, nil
}

// UpsertPublicPoolShare appends or replaces the PublicPoolShare
// entry for `poolIdx` on the master account. Used by MintShares to
// fold a fresh deposit into the LP row.
//
// Returns ErrSharesListFull when no entry exists and the per-master
// share-list cap is reached.
func (k Keeper) UpsertPublicPoolShare(
	ctx context.Context,
	masterIdx, poolIdx uint64,
	shareDelta, principalDelta math.Int,
	entryTimestamp int64,
) error {
	master, err := k.GetAccount(ctx, masterIdx)
	if err != nil {
		return err
	}
	if i, ok := FindShareEntry(master, poolIdx); ok {
		master.PublicPoolShares[i].ShareAmount = master.PublicPoolShares[i].ShareAmount.Add(shareDelta)
		master.PublicPoolShares[i].PrincipalAmount = master.PublicPoolShares[i].PrincipalAmount.Add(principalDelta)
		master.PublicPoolShares[i].EntryTimestamp = entryTimestamp
	} else {
		if uint32(len(master.PublicPoolShares)) >= uint32(perptypes.SharesListSize) {
			return types.ErrSharesListFull
		}
		master.PublicPoolShares = append(master.PublicPoolShares, types.PublicPoolShare{
			PublicPoolIndex: poolIdx,
			ShareAmount:     shareDelta,
			PrincipalAmount: principalDelta,
			EntryTimestamp:  entryTimestamp,
		})
	}
	return k.updateAccount(ctx, master)
}

// ReducePublicPoolShare debits `shareAmount` (and the proportional
// principal) from the master's existing PublicPoolShare for
// `poolIdx`. When the resulting `ShareAmount` reaches zero the entry
// is removed entirely. Used by BurnShares.
//
// Caller must have already validated the entry exists with at least
// `shareAmount` shares (ErrInsufficientShares is the standard
// surface for a missing/under-funded entry).
func (k Keeper) ReducePublicPoolShare(
	ctx context.Context,
	masterIdx, poolIdx uint64,
	shareAmount math.Int,
) error {
	master, err := k.GetAccount(ctx, masterIdx)
	if err != nil {
		return err
	}
	entryIdx, ok := FindShareEntry(master, poolIdx)
	if !ok {
		return types.ErrInsufficientShares.Wrap("depositor has no entry for this pool")
	}
	entry := master.PublicPoolShares[entryIdx]
	if entry.ShareAmount.LT(shareAmount) {
		return types.ErrInsufficientShares.Wrapf(
			"requested %s, have %s",
			shareAmount.String(), entry.ShareAmount.String(),
		)
	}
	principalDelta := entry.PrincipalAmount.Mul(shareAmount).Quo(entry.ShareAmount)
	entry.ShareAmount = entry.ShareAmount.Sub(shareAmount)
	entry.PrincipalAmount = entry.PrincipalAmount.Sub(principalDelta)
	if entry.ShareAmount.IsZero() {
		master.PublicPoolShares = append(master.PublicPoolShares[:entryIdx], master.PublicPoolShares[entryIdx+1:]...)
	} else {
		master.PublicPoolShares[entryIdx] = entry
	}
	return k.updateAccount(ctx, master)
}

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

// BurnAllowed reports whether the pool's current status permits a
// share burn. ACTIVE and FROZEN both allow burn — FROZEN is the
// wind-down state and LPs MUST be able to exit. Any future status
// (e.g. an in-flight migration) is rejected by default so that adding
// a new state can never silently widen the burn surface.
func BurnAllowed(info types.PublicPoolInfo) bool {
	return info.Status == perptypes.PublicPoolStatusActive ||
		info.Status == perptypes.PublicPoolStatusFrozen
}

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
