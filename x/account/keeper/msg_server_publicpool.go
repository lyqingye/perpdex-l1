package keeper

import (
	"context"
	"fmt"
	"strconv"

	"cosmossdk.io/collections"
	"cosmossdk.io/math"

	sdk "github.com/cosmos/cosmos-sdk/types"

	perptypes "github.com/perpdex/perpdex-l1/types"
	"github.com/perpdex/perpdex-l1/x/account/types"
)

// ---------- CreatePublicPool ----------

// CreatePublicPool creates a new pool sub-account whose master is
// `msg.MasterAccountIndex`. The master pays
// `initial_total_shares * INITIAL_POOL_SHARE_VALUE * USDC_TO_COLLATERAL`
// in collateral, which becomes the seed collateral of the pool.
//
// Mirrors lighter `circuit/src/transactions/l2_create_public_pool.rs`:
// when master == InsuranceFundOperatorAccountIdx the new pool is
// INSURANCE_FUND + UNIFIED; otherwise PUBLIC_POOL + SIMPLE.
func (m msgServer) CreatePublicPool(ctx context.Context, msg *types.MsgCreatePublicPool) (*types.MsgCreatePublicPoolResponse, error) {
	if msg.InitialTotalShares == 0 {
		return nil, types.ErrInvalidParams.Wrap("initial_total_shares must be > 0")
	}
	// MsgCreatePublicPool only spawns regular PUBLIC_POOL pools. The
	// canonical Insurance Fund pool lives at
	// InsuranceFundOperatorAccountIdx and is wired by genesis; its
	// state is mutated via MsgUpdatePublicPool / MsgStrategyTransfer
	// authenticated by gov authority.
	if msg.AccountType != perptypes.PublicPoolAccountType {
		return nil, types.ErrInvalidAccountType.Wrapf(
			"account_type must be PUBLIC_POOL(%d); IF pool is genesis-only",
			perptypes.PublicPoolAccountType,
		)
	}
	if msg.OperatorFee >= uint32(perptypes.FeeTick) {
		return nil, types.ErrInvalidParams.Wrapf(
			"operator_fee must be < FeeTick(%d)", perptypes.FeeTick,
		)
	}
	if msg.MinOperatorShareRate > perptypes.ShareTick {
		return nil, types.ErrInvalidParams.Wrapf(
			"min_operator_share_rate must be <= ShareTick(%d)", perptypes.ShareTick,
		)
	}

	master, err := m.GetAccount(ctx, msg.MasterAccountIndex)
	if err != nil {
		return nil, err
	}
	if master.OwnerAddress == "" || master.OwnerAddress != msg.Sender {
		return nil, types.ErrUnauthorized
	}
	if master.AccountType != perptypes.MasterAccountType {
		return nil, types.ErrInvalidAccountType.Wrap("master is not a master account")
	}

	resolvedType := perptypes.PublicPoolAccountType
	resolvedMode := perptypes.AccountTradingModeSimple

	// Compute seed collateral = initial_total_shares * INITIAL_POOL_SHARE_VALUE * USDC_TO_COLLATERAL.
	seedUSDC := math.NewIntFromUint64(msg.InitialTotalShares).
		Mul(math.NewIntFromUint64(perptypes.InitialPoolShareValue))
	seedCollat := seedUSDC.Mul(math.NewIntFromUint64(perptypes.USDCToCollateralMultiplier))

	// Pre-flight: master must have enough collateral.
	if master.Collateral.IsNil() {
		master.Collateral = math.ZeroInt()
	}
	if master.Collateral.LT(seedCollat) {
		return nil, types.ErrInsufficientFunds.Wrapf(
			"need %s, have %s", seedCollat.String(), master.Collateral.String(),
		)
	}

	// Allocate pool sub-account index.
	idx, err := m.allocatePoolSubAccountIndex(ctx)
	if err != nil {
		return nil, err
	}

	now := sdk.UnwrapSDKContext(ctx).BlockTime().UnixMilli()
	zeros := make([]math.Int, perptypes.NbStrategies)
	for i := range zeros {
		zeros[i] = math.ZeroInt()
	}
	pool := types.Account{
		AccountIndex:       idx,
		MasterAccountIndex: master.AccountIndex,
		OwnerAddress:       master.OwnerAddress,
		AccountType:        resolvedType,
		AccountTradingMode: resolvedMode,
		Collateral:         seedCollat,
		CreatedAt:          now,
		PublicPoolInfo: &types.PublicPoolInfo{
			Status:               perptypes.PublicPoolStatusActive,
			OperatorFee:          msg.OperatorFee,
			MinOperatorShareRate: msg.MinOperatorShareRate,
			TotalShares:          math.NewIntFromUint64(msg.InitialTotalShares),
			OperatorShares:       math.NewIntFromUint64(msg.InitialTotalShares),
			Strategies:           zeros,
		},
	}
	if err := m.SetAccount(ctx, pool); err != nil {
		return nil, err
	}
	if err := m.AddCollateral(ctx, master.AccountIndex, seedCollat.Neg()); err != nil {
		return nil, err
	}

	sdk.UnwrapSDKContext(ctx).EventManager().EmitEvent(sdk.NewEvent(
		"create_public_pool",
		sdk.NewAttribute("pool_account_index", strconv.FormatUint(idx, 10)),
		sdk.NewAttribute("master_account_index", strconv.FormatUint(master.AccountIndex, 10)),
		sdk.NewAttribute("account_type", strconv.FormatUint(uint64(resolvedType), 10)),
		sdk.NewAttribute("initial_total_shares", strconv.FormatUint(msg.InitialTotalShares, 10)),
	))
	return &types.MsgCreatePublicPoolResponse{PoolAccountIndex: idx}, nil
}

// allocatePoolSubAccountIndex pulls the next sub-account index, skipping
// any reserved indexes. Mirrors CreateSubAccount's allocation logic
// without the master-type guard so that the IF master (which carries
// nil owner) can still spawn sub-accounts.
func (m msgServer) allocatePoolSubAccountIndex(ctx context.Context) (uint64, error) {
	idx, err := m.NextSubIndex.Next(ctx)
	if err != nil {
		return 0, err
	}
	for idx < perptypes.MinSubAccountIndex {
		idx, err = m.NextSubIndex.Next(ctx)
		if err != nil {
			return 0, err
		}
	}
	if idx > perptypes.MaxAccountIndex {
		return 0, types.ErrAccountIndexExceed.Wrapf("sub idx=%d", idx)
	}
	return idx, nil
}

// ---------- UpdatePublicPool ----------

// UpdatePublicPool flips status / operator_fee / min_operator_share_rate.
// Sender must be the pool's master owner. The pool must be ACTIVE
// (frozen pools are read-only). To FREEZE the pool, it must be
// healthy + position-free + no open orders. Freezing the LLP also
// clears the system Params.LiquidityPoolIndex (lighter parity).
func (m msgServer) UpdatePublicPool(ctx context.Context, msg *types.MsgUpdatePublicPool) (*types.MsgUpdatePublicPoolResponse, error) {
	pool, err := m.GetAccount(ctx, msg.PoolAccountIndex)
	if err != nil {
		return nil, err
	}
	if !IsPoolAccount(pool) {
		return nil, types.ErrInvalidPoolAccount
	}
	if err := EnsureActive(pool.PublicPoolInfo); err != nil {
		return nil, err
	}
	if err := m.assertPoolOperator(ctx, pool, msg.Sender); err != nil {
		return nil, err
	}

	if msg.NewStatus != perptypes.PublicPoolStatusActive &&
		msg.NewStatus != perptypes.PublicPoolStatusFrozen {
		return nil, types.ErrInvalidPoolUpdate.Wrapf("unknown status %d", msg.NewStatus)
	}
	// operator_fee can only DECREASE.
	if msg.NewOperatorFee > pool.PublicPoolInfo.OperatorFee {
		return nil, types.ErrInvalidPoolUpdate.Wrapf(
			"operator_fee can only decrease (old=%d new=%d)",
			pool.PublicPoolInfo.OperatorFee, msg.NewOperatorFee,
		)
	}
	if msg.NewMinOperatorShareRate > perptypes.ShareTick {
		return nil, types.ErrInvalidParams.Wrapf(
			"min_operator_share_rate must be <= ShareTick(%d)", perptypes.ShareTick,
		)
	}

	// Build candidate updated info to test min_rate invariant.
	candidate := *pool.PublicPoolInfo
	candidate.MinOperatorShareRate = msg.NewMinOperatorShareRate
	if !CheckMinOperatorShareRate(candidate) {
		return nil, types.ErrOperatorRateViolation
	}

	// Freeze gate: pool must be HEALTHY + no positions + no open orders.
	if msg.NewStatus == perptypes.PublicPoolStatusFrozen {
		if err := m.assertPoolEmpty(ctx, pool); err != nil {
			return nil, err
		}
	}

	pool.PublicPoolInfo.Status = msg.NewStatus
	pool.PublicPoolInfo.OperatorFee = msg.NewOperatorFee
	pool.PublicPoolInfo.MinOperatorShareRate = msg.NewMinOperatorShareRate
	if err := m.SetAccount(ctx, pool); err != nil {
		return nil, err
	}

	// If we just froze the LLP pool, clear the system pointer.
	if msg.NewStatus == perptypes.PublicPoolStatusFrozen {
		params, err := m.Params.Get(ctx)
		if err != nil {
			return nil, err
		}
		if params.LiquidityPoolIndex == pool.AccountIndex {
			params.LiquidityPoolIndex = 0
			if err := m.Params.Set(ctx, params); err != nil {
				return nil, err
			}
		}
	}

	sdk.UnwrapSDKContext(ctx).EventManager().EmitEvent(sdk.NewEvent(
		"update_public_pool",
		sdk.NewAttribute("pool_account_index", strconv.FormatUint(pool.AccountIndex, 10)),
		sdk.NewAttribute("status", strconv.FormatUint(uint64(msg.NewStatus), 10)),
		sdk.NewAttribute("operator_fee", strconv.FormatUint(uint64(msg.NewOperatorFee), 10)),
	))
	return &types.MsgUpdatePublicPoolResponse{}, nil
}

// assertPoolEmpty verifies the pool is HEALTHY + has no perp position
// in any market + holds no open orders (TotalOrderCount == 0).
func (m msgServer) assertPoolEmpty(ctx context.Context, pool types.Account) error {
	status, err := m.riskKeeper.GetHealthStatus(ctx, pool.AccountIndex)
	if err != nil {
		return err
	}
	if status != perptypes.HealthHealthy {
		return types.ErrPoolMustBeEmpty.Wrapf("pool not healthy (status=%d)", status)
	}
	if pool.TotalOrderCount != 0 {
		return types.ErrPoolMustBeEmpty.Wrapf(
			"pool has %d open order(s)", pool.TotalOrderCount,
		)
	}
	// Walk positions for this pool index.
	pref := collections.NewPrefixedPairRange[uint64, uint32](pool.AccountIndex)
	iter, err := m.AccountPositions.Iterate(ctx, pref)
	if err != nil {
		return err
	}
	defer iter.Close()
	for ; iter.Valid(); iter.Next() {
		p, err := iter.Value()
		if err != nil {
			return err
		}
		if !p.Position.IsNil() && !p.Position.IsZero() {
			return types.ErrPoolMustBeEmpty.Wrapf(
				"pool has non-zero position in market %d", p.MarketIndex,
			)
		}
	}
	return nil
}

// ---------- helpers shared by mint/burn ----------

// resolveSenderMaster returns sender's master account, used by mint/burn
// to identify the LP account row for the share entry.
func (m msgServer) resolveSenderMaster(ctx context.Context, sender string) (types.Account, error) {
	master, err := m.GetMasterAccountByOwner(ctx, sender)
	if err != nil {
		return types.Account{}, fmt.Errorf("sender has no perpdex account: %w", err)
	}
	return master, nil
}

// assertPoolOperator returns nil iff `sender` is the legitimate
// operator of `pool`. For regular PUBLIC_POOL accounts the operator is
// the master account's owner. For the canonical IF pool (genesis
// account 1, no master, no owner) the operator is the chain's gov
// authority.
func (m msgServer) assertPoolOperator(ctx context.Context, pool types.Account, sender string) error {
	if pool.AccountType == perptypes.InsuranceFundAccountType &&
		pool.AccountIndex == perptypes.InsuranceFundOperatorAccountIdx {
		if sender != m.authority {
			return types.ErrUnauthorized.Wrap("IF pool operator is gov authority")
		}
		return nil
	}
	master, err := m.GetAccount(ctx, pool.MasterAccountIndex)
	if err != nil {
		return err
	}
	if master.OwnerAddress == "" || master.OwnerAddress != sender {
		return types.ErrUnauthorized
	}
	return nil
}

// isPoolOperator returns true when `sender` controls the pool.
// Used by Mint to decide whether the deposit goes into operator_shares.
func (m msgServer) isPoolOperator(ctx context.Context, pool types.Account, sender string) (bool, error) {
	if pool.AccountType == perptypes.InsuranceFundAccountType &&
		pool.AccountIndex == perptypes.InsuranceFundOperatorAccountIdx {
		return sender == m.authority, nil
	}
	master, err := m.GetAccount(ctx, pool.MasterAccountIndex)
	if err != nil {
		return false, err
	}
	if master.OwnerAddress == "" {
		return false, nil
	}
	return master.OwnerAddress == sender, nil
}

// ---------- MintShares ----------

// MintShares deposits sender's master collateral into the pool and
// allocates fresh shares at the current NAV. Mirrors lighter
// `l2_mint_shares.rs`: principal is the cumulative cost-basis used by
// future burn profit calc; entry_timestamp is reset on every non-
// operator mint and gates the LLP burn cooldown.
func (m msgServer) MintShares(ctx context.Context, msg *types.MsgMintShares) (*types.MsgMintSharesResponse, error) {
	pool, err := m.GetAccount(ctx, msg.PoolAccountIndex)
	if err != nil {
		return nil, err
	}
	if !IsPoolAccount(pool) {
		return nil, types.ErrInvalidPoolAccount
	}
	if err := EnsureActive(pool.PublicPoolInfo); err != nil {
		return nil, err
	}

	master, err := m.resolveSenderMaster(ctx, msg.Sender)
	if err != nil {
		return nil, err
	}
	isOperator, err := m.isPoolOperator(ctx, pool, msg.Sender)
	if err != nil {
		return nil, err
	}

	usdc := math.NewIntFromUint64(msg.PrincipalAmount)
	collatDelta := usdc.Mul(math.NewIntFromUint64(perptypes.USDCToCollateralMultiplier))

	if master.Collateral.IsNil() {
		master.Collateral = math.ZeroInt()
	}
	if master.Collateral.LT(collatDelta) {
		return nil, types.ErrInsufficientFunds.Wrapf(
			"need %s, have %s", collatDelta.String(), master.Collateral.String(),
		)
	}

	shareAmount, err := m.USDCValueToShares(ctx, pool.AccountIndex, usdc)
	if err != nil {
		return nil, err
	}
	if !shareAmount.IsPositive() {
		return nil, types.ErrInvalidParams.Wrap("computed share amount is zero")
	}

	// Move funds: master.collateral -> pool.collateral.
	if err := m.AddCollateral(ctx, master.AccountIndex, collatDelta.Neg()); err != nil {
		return nil, err
	}
	if err := m.AddCollateral(ctx, pool.AccountIndex, collatDelta); err != nil {
		return nil, err
	}

	// Re-fetch pool after collateral change to keep info in sync.
	pool, err = m.GetAccount(ctx, pool.AccountIndex)
	if err != nil {
		return nil, err
	}
	info := pool.PublicPoolInfo
	info.TotalShares = info.TotalShares.Add(shareAmount)
	if isOperator {
		info.OperatorShares = info.OperatorShares.Add(shareAmount)
	} else {
		// Non-operator mint may not break min_operator_share_rate.
		if !CheckMinOperatorShareRate(*info) {
			return nil, types.ErrOperatorRateViolation
		}
	}
	pool.PublicPoolInfo = info
	if err := m.SetAccount(ctx, pool); err != nil {
		return nil, err
	}

	// Update LP-side share entry on master row.
	if !isOperator {
		master, err = m.GetAccount(ctx, master.AccountIndex)
		if err != nil {
			return nil, err
		}
		now := sdk.UnwrapSDKContext(ctx).BlockTime().UnixMilli()
		if i, ok := FindShareEntry(master, pool.AccountIndex); ok {
			master.PublicPoolShares[i].ShareAmount = master.PublicPoolShares[i].ShareAmount.Add(shareAmount)
			master.PublicPoolShares[i].PrincipalAmount = master.PublicPoolShares[i].PrincipalAmount.Add(usdc)
			master.PublicPoolShares[i].EntryTimestamp = now
		} else {
			if uint32(len(master.PublicPoolShares)) >= uint32(perptypes.SharesListSize) {
				return nil, types.ErrSharesListFull
			}
			master.PublicPoolShares = append(master.PublicPoolShares, types.PublicPoolShare{
				PublicPoolIndex: pool.AccountIndex,
				ShareAmount:     shareAmount,
				PrincipalAmount: usdc,
				EntryTimestamp:  now,
			})
		}
		if err := m.SetAccount(ctx, master); err != nil {
			return nil, err
		}
	}

	sdk.UnwrapSDKContext(ctx).EventManager().EmitEvent(sdk.NewEvent(
		"mint_shares",
		sdk.NewAttribute("pool_account_index", strconv.FormatUint(pool.AccountIndex, 10)),
		sdk.NewAttribute("sender_master", strconv.FormatUint(master.AccountIndex, 10)),
		sdk.NewAttribute("share_amount", shareAmount.String()),
		sdk.NewAttribute("principal_amount", usdc.String()),
	))
	return &types.MsgMintSharesResponse{ShareAmount: shareAmount}, nil
}

// ---------- BurnShares ----------

// BurnShares redeems sender's shares from the pool back to USDC.
// Cooldown applies to LLP + non-operator burns. operator_fee_share
// (computed as in lighter `l2_burn_shares.rs`) is split out from
// realised profit and credited to operator_shares.
func (m msgServer) BurnShares(ctx context.Context, msg *types.MsgBurnShares) (*types.MsgBurnSharesResponse, error) {
	resp, err := m.burnSharesCore(ctx, msg.Sender, msg.PoolAccountIndex, msg.ShareAmount, false /* skipCooldown */, false /* asAuthority */)
	if err != nil {
		return nil, err
	}
	return resp, nil
}

// burnSharesCore is shared by BurnShares and ForceBurnShares.
//
//	skipCooldown   -> bypass LLP cooldown (force-burn path)
//	asAuthority    -> caller is gov authority; the `sender` arg is in
//	                  fact the depositor master index (encoded as
//	                  string for ergonomics; we pass owner addr instead
//	                  via lookup elsewhere).
func (m msgServer) burnSharesCore(
	ctx context.Context,
	owner string,
	poolIdx uint64,
	shareAmount math.Int,
	skipCooldown bool,
	asAuthority bool,
) (*types.MsgBurnSharesResponse, error) {
	pool, err := m.GetAccount(ctx, poolIdx)
	if err != nil {
		return nil, err
	}
	if !IsPoolAccount(pool) {
		return nil, types.ErrInvalidPoolAccount
	}

	// Find depositor master.
	var depositor types.Account
	if asAuthority {
		// In the ForceBurn flow, owner is actually the depositor's
		// master AccountIndex serialized as ASCII; the wrapper passes
		// it through this slot.
		depIdx, err := strconv.ParseUint(owner, 10, 64)
		if err != nil {
			return nil, fmt.Errorf("invalid depositor index: %w", err)
		}
		depositor, err = m.GetAccount(ctx, depIdx)
		if err != nil {
			return nil, err
		}
	} else {
		depositor, err = m.resolveSenderMaster(ctx, owner)
		if err != nil {
			return nil, err
		}
	}

	isOperator := false
	if !asAuthority {
		isOperator, err = m.isPoolOperator(ctx, pool, owner)
		if err != nil {
			return nil, err
		}
	}

	// Pool must be healthy enough to burn (lighter requires non-
	// liquidation cross risk). We approximate by requiring TAV > 0,
	// which AvailableSharesToBurn already enforces.
	available, err := m.AvailableSharesToBurn(ctx, poolIdx)
	if err != nil {
		return nil, err
	}
	if shareAmount.GT(available) {
		return nil, types.ErrInsufficientShares.Wrapf(
			"requested %s exceeds available burn cap %s",
			shareAmount.String(), available.String(),
		)
	}

	info := pool.PublicPoolInfo
	frozen := info.Status == perptypes.PublicPoolStatusFrozen

	// Locate share entry on depositor row (operator burns from
	// info.OperatorShares directly without a per-row entry).
	var entryIdx int
	var hasEntry bool
	if !isOperator {
		entryIdx, hasEntry = FindShareEntry(depositor, poolIdx)
		if !hasEntry {
			return nil, types.ErrInsufficientShares.Wrap("depositor has no entry for this pool")
		}
		if depositor.PublicPoolShares[entryIdx].ShareAmount.LT(shareAmount) {
			return nil, types.ErrInsufficientShares.Wrapf(
				"requested %s, have %s",
				shareAmount.String(),
				depositor.PublicPoolShares[entryIdx].ShareAmount.String(),
			)
		}
	} else {
		if info.OperatorShares.LT(shareAmount) {
			return nil, types.ErrInsufficientShares.Wrap("operator shares insufficient")
		}
		// Non-frozen operator burn must keep min_operator_share_rate.
		// We test with the candidate post-state below.
	}

	// LLP cooldown: only for non-operator burn on the system LLP pool.
	if !isOperator && !skipCooldown {
		params, err := m.Params.Get(ctx)
		if err != nil {
			return nil, err
		}
		if params.LiquidityPoolIndex == poolIdx && params.LiquidityPoolCooldownPeriodMs > 0 {
			now := sdk.UnwrapSDKContext(ctx).BlockTime().UnixMilli()
			earliest := depositor.PublicPoolShares[entryIdx].EntryTimestamp + params.LiquidityPoolCooldownPeriodMs
			if now < earliest {
				return nil, types.ErrCooldownNotElapsed.Wrapf(
					"earliest_burn_at_ms=%d now_ms=%d", earliest, now,
				)
			}
		}
	}

	// Compute pre-fee redemption USDC value.
	usdcValue, err := m.SharesToUSDCValue(ctx, poolIdx, shareAmount)
	if err != nil {
		return nil, err
	}
	if !usdcValue.IsPositive() {
		return nil, types.ErrInvalidParams.Wrap("computed redeem usdc is zero")
	}

	// Realised profit + operator fee math (lighter parity, applies
	// only to non-operator burns).
	burnedShares := shareAmount
	operatorFeeShares := math.ZeroInt()
	if !isOperator {
		ownedShares := depositor.PublicPoolShares[entryIdx].ShareAmount
		entryUSDC := depositor.PublicPoolShares[entryIdx].PrincipalAmount
		// usdc_paid_for_shares = entry_usdc * share_amount / owned_shares
		usdcPaid := entryUSDC.Mul(shareAmount).Quo(ownedShares)
		if usdcValue.GT(usdcPaid) && info.OperatorFee > 0 {
			tav, err := m.riskKeeper.GetTotalAccountValue(ctx, poolIdx)
			if err != nil {
				return nil, err
			}
			if tav.IsPositive() {
				profit := usdcValue.Sub(usdcPaid)
				// fee_shares = profit * operator_fee * total_shares * USDC_TO_COLLATERAL / (FEE_TICK * TAV)
				num := profit.
					Mul(math.NewIntFromUint64(uint64(info.OperatorFee))).
					Mul(info.TotalShares).
					Mul(math.NewIntFromUint64(perptypes.USDCToCollateralMultiplier))
				denom := math.NewIntFromUint64(perptypes.FeeTick).Mul(tav)
				if denom.IsPositive() {
					operatorFeeShares = num.Quo(denom)
					if operatorFeeShares.GT(shareAmount) {
						operatorFeeShares = shareAmount
					}
				}
			}
		}
		burnedShares = shareAmount.Sub(operatorFeeShares)
	}

	// Redeem-side: re-quote with the post-fee-cut share count to
	// determine the USDC delivered to the depositor.
	deliveredUSDC, err := m.SharesToUSDCValue(ctx, poolIdx, burnedShares)
	if err != nil {
		return nil, err
	}
	deliveredCollat := deliveredUSDC.Mul(math.NewIntFromUint64(perptypes.USDCToCollateralMultiplier))

	// Mutate pool.public_pool_info.
	info.TotalShares = info.TotalShares.Sub(burnedShares)
	if isOperator {
		info.OperatorShares = info.OperatorShares.Sub(shareAmount)
	} else {
		// Non-frozen operator-rate invariant after a non-operator burn:
		// the operator floor cannot have been violated since
		// total_shares only decreased; still re-check defensively.
		// Operator-fee shares are awarded to the operator.
		info.OperatorShares = info.OperatorShares.Add(operatorFeeShares)
	}
	// Non-frozen pools must always respect the operator floor, including
	// when an operator burn drove OperatorShares to zero (previously the
	// IsPositive guard let that edge case bypass the check, letting the
	// operator withdraw their skin-in-the-game entirely).
	if !frozen && !CheckMinOperatorShareRate(*info) {
		return nil, types.ErrOperatorRateViolation
	}

	// Re-fetch & write pool with updated info + reduced collateral.
	if err := m.AddCollateral(ctx, poolIdx, deliveredCollat.Neg()); err != nil {
		return nil, err
	}
	pool, err = m.GetAccount(ctx, poolIdx)
	if err != nil {
		return nil, err
	}
	pool.PublicPoolInfo = info
	if err := m.SetAccount(ctx, pool); err != nil {
		return nil, err
	}

	// Credit depositor's master collateral.
	if err := m.AddCollateral(ctx, depositor.AccountIndex, deliveredCollat); err != nil {
		return nil, err
	}

	// Update depositor share entry / principal_amount.
	if !isOperator {
		depositor, err = m.GetAccount(ctx, depositor.AccountIndex)
		if err != nil {
			return nil, err
		}
		entryIdx, _ = FindShareEntry(depositor, poolIdx)
		entry := depositor.PublicPoolShares[entryIdx]
		// principal_delta = entry.principal * share_amount / owned_shares
		principalDelta := entry.PrincipalAmount.Mul(shareAmount).Quo(entry.ShareAmount)
		entry.ShareAmount = entry.ShareAmount.Sub(shareAmount)
		entry.PrincipalAmount = entry.PrincipalAmount.Sub(principalDelta)
		if entry.ShareAmount.IsZero() {
			depositor.PublicPoolShares = append(depositor.PublicPoolShares[:entryIdx], depositor.PublicPoolShares[entryIdx+1:]...)
		} else {
			depositor.PublicPoolShares[entryIdx] = entry
		}
		if err := m.SetAccount(ctx, depositor); err != nil {
			return nil, err
		}
	}

	sdk.UnwrapSDKContext(ctx).EventManager().EmitEvent(sdk.NewEvent(
		"burn_shares",
		sdk.NewAttribute("pool_account_index", strconv.FormatUint(poolIdx, 10)),
		sdk.NewAttribute("depositor", strconv.FormatUint(depositor.AccountIndex, 10)),
		sdk.NewAttribute("share_amount", shareAmount.String()),
		sdk.NewAttribute("operator_fee_shares", operatorFeeShares.String()),
		sdk.NewAttribute("usdc_amount", deliveredUSDC.String()),
	))
	return &types.MsgBurnSharesResponse{UsdcAmount: deliveredUSDC.Uint64()}, nil
}

// ---------- StrategyTransfer ----------

// StrategyTransfer reallocates collateral between IF strategy buckets.
// Mirrors lighter `l2_strategy_transfer.rs`: pool must be IF, non-
// frozen, sender must be the pool operator, from != to, and the from
// bucket must hold the requested amount.
func (m msgServer) StrategyTransfer(ctx context.Context, msg *types.MsgStrategyTransfer) (*types.MsgStrategyTransferResponse, error) {
	pool, err := m.GetAccount(ctx, msg.PoolAccountIndex)
	if err != nil {
		return nil, err
	}
	if !IsPoolAccount(pool) {
		return nil, types.ErrInvalidPoolAccount
	}
	if pool.AccountType != perptypes.InsuranceFundAccountType {
		return nil, types.ErrNotInsuranceFund
	}
	if err := m.assertPoolOperator(ctx, pool, msg.Sender); err != nil {
		return nil, err
	}
	if err := EnsureNotFrozen(pool.PublicPoolInfo); err != nil {
		return nil, err
	}
	if msg.FromStrategy >= uint32(perptypes.NbStrategies) ||
		msg.ToStrategy >= uint32(perptypes.NbStrategies) {
		return nil, types.ErrInvalidStrategyIdx
	}
	if len(pool.PublicPoolInfo.Strategies) != perptypes.NbStrategies {
		// Defensive: rebuild zeros if migration ever drifts the slot count.
		fixed := make([]math.Int, perptypes.NbStrategies)
		for i := range fixed {
			fixed[i] = math.ZeroInt()
		}
		copy(fixed, pool.PublicPoolInfo.Strategies)
		pool.PublicPoolInfo.Strategies = fixed
	}
	from := pool.PublicPoolInfo.Strategies[msg.FromStrategy]
	if from.IsNil() {
		from = math.ZeroInt()
	}
	if from.LT(msg.Amount) {
		return nil, types.ErrInsufficientFunds.Wrapf(
			"strategy[%d] has %s, need %s",
			msg.FromStrategy, from.String(), msg.Amount.String(),
		)
	}
	to := pool.PublicPoolInfo.Strategies[msg.ToStrategy]
	if to.IsNil() {
		to = math.ZeroInt()
	}
	pool.PublicPoolInfo.Strategies[msg.FromStrategy] = from.Sub(msg.Amount)
	pool.PublicPoolInfo.Strategies[msg.ToStrategy] = to.Add(msg.Amount)
	if err := m.SetAccount(ctx, pool); err != nil {
		return nil, err
	}

	sdk.UnwrapSDKContext(ctx).EventManager().EmitEvent(sdk.NewEvent(
		"strategy_transfer",
		sdk.NewAttribute("pool_account_index", strconv.FormatUint(pool.AccountIndex, 10)),
		sdk.NewAttribute("from", strconv.FormatUint(uint64(msg.FromStrategy), 10)),
		sdk.NewAttribute("to", strconv.FormatUint(uint64(msg.ToStrategy), 10)),
		sdk.NewAttribute("amount", msg.Amount.String()),
	))
	return &types.MsgStrategyTransferResponse{}, nil
}

// ---------- ForceBurnShares ----------

// ForceBurnShares is the gov-authority escape hatch on the LLP pool.
// Bypasses the cooldown, otherwise mirrors BurnShares.
func (m msgServer) ForceBurnShares(ctx context.Context, msg *types.MsgForceBurnShares) (*types.MsgForceBurnSharesResponse, error) {
	if msg.Authority != m.authority {
		return nil, types.ErrInvalidAuthority
	}
	pool, err := m.GetAccount(ctx, msg.PoolAccountIndex)
	if err != nil {
		return nil, err
	}
	if !IsPoolAccount(pool) {
		return nil, types.ErrInvalidPoolAccount
	}
	if pool.AccountType != perptypes.InsuranceFundAccountType {
		return nil, types.ErrNotInsuranceFund
	}
	params, err := m.Params.Get(ctx)
	if err != nil {
		return nil, err
	}
	if params.LiquidityPoolIndex != msg.PoolAccountIndex {
		return nil, types.ErrInvalidPoolUpdate.Wrap("pool is not the canonical LLP")
	}

	// Encode depositor index in the `owner` slot to reuse burnSharesCore.
	depositorRef := strconv.FormatUint(msg.DepositorAccountIndex, 10)
	resp, err := m.burnSharesCore(ctx, depositorRef, msg.PoolAccountIndex, msg.ShareAmount, true /* skipCooldown */, true /* asAuthority */)
	if err != nil {
		return nil, err
	}
	return &types.MsgForceBurnSharesResponse{UsdcAmount: resp.UsdcAmount}, nil
}
