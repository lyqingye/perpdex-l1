package keeper

import (
	"context"
	"strconv"

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
// When master == InsuranceFundOperatorAccountIdx the new pool is
// INSURANCE_FUND + UNIFIED; otherwise PUBLIC_POOL + SIMPLE.
func (m msgServer) CreatePublicPool(ctx context.Context, msg *types.MsgCreatePublicPool) (*types.MsgCreatePublicPoolResponse, error) {
	if err := msg.ValidateBasic(); err != nil {
		return nil, err
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
	if master.Collateral.LT(seedCollat) {
		return nil, types.ErrInsufficientFunds.Wrapf(
			"need %s, have %s", seedCollat.String(), master.Collateral.String(),
		)
	}

	// Seeding a pool removes margin from the master, so settle pending
	// funding and snapshot pre-state risk before mutating collateral.
	// This matches the Withdraw / Transfer template and prevents a
	// position-holding master from siphoning maintenance margin into a
	// brand-new pool.
	if err := m.settleAllPositionFunding(ctx, master.AccountIndex); err != nil {
		return nil, err
	}
	pre, err := m.snapshotPreRisk(ctx, master.AccountIndex)
	if err != nil {
		return nil, err
	}

	zeros := make([]math.Int, perptypes.NbStrategies)
	for i := range zeros {
		zeros[i] = math.ZeroInt()
	}
	pool, err := m.CreatePublicPoolAccount(ctx, PublicPoolAccountParams{
		Master:             master,
		AccountType:        resolvedType,
		AccountTradingMode: resolvedMode,
		SeedCollateral:     seedCollat,
		Info: &types.PublicPoolInfo{
			Status:               perptypes.PublicPoolStatusActive,
			OperatorFee:          msg.OperatorFee,
			MinOperatorShareRate: msg.MinOperatorShareRate,
			TotalShares:          math.NewIntFromUint64(msg.InitialTotalShares),
			OperatorShares:       math.NewIntFromUint64(msg.InitialTotalShares),
			Strategies:           zeros,
		},
	})
	if err != nil {
		return nil, err
	}
	if err := m.AddCollateral(ctx, master.AccountIndex, seedCollat.Neg()); err != nil {
		return nil, err
	}
	if err := m.requireRiskOKFrom(ctx, master.AccountIndex, pre); err != nil {
		return nil, err
	}

	sdk.UnwrapSDKContext(ctx).EventManager().EmitEvent(sdk.NewEvent(
		types.EventTypeCreatePublicPool,
		sdk.NewAttribute(types.AttributeKeyPoolAccountIndex, strconv.FormatUint(pool.AccountIndex, 10)),
		sdk.NewAttribute(types.AttributeKeyMasterAccountIndex, strconv.FormatUint(master.AccountIndex, 10)),
		sdk.NewAttribute(types.AttributeKeyAccountType, strconv.FormatUint(uint64(resolvedType), 10)),
		sdk.NewAttribute(types.AttributeKeyInitialTotalShares, strconv.FormatUint(msg.InitialTotalShares, 10)),
	))
	return &types.MsgCreatePublicPoolResponse{PoolAccountIndex: pool.AccountIndex}, nil
}

// ---------- UpdatePublicPool ----------

// UpdatePublicPool flips status / operator_fee / min_operator_share_rate.
// Sender must be the pool's master owner. The pool must be ACTIVE
// (frozen pools are read-only). To FREEZE the pool, it must be
// healthy + position-free + no open orders. Freezing the LLP also
// clears the system Params.LiquidityPoolIndex.
func (m msgServer) UpdatePublicPool(ctx context.Context, msg *types.MsgUpdatePublicPool) (*types.MsgUpdatePublicPoolResponse, error) {
	if err := msg.ValidateBasic(); err != nil {
		return nil, err
	}
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

	// new_status enum and new_min_operator_share_rate upper bound are
	// already checked in MsgUpdatePublicPool.ValidateBasic.
	// operator_fee monotonic-decrease still requires reading the pool.
	if msg.NewOperatorFee > pool.PublicPoolInfo.OperatorFee {
		return nil, types.ErrInvalidPoolUpdate.Wrapf(
			"operator_fee can only decrease (old=%d new=%d)",
			pool.PublicPoolInfo.OperatorFee, msg.NewOperatorFee,
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

	if _, err := m.UpdatePublicPoolInfo(ctx, pool.AccountIndex, func(info *types.PublicPoolInfo) error {
		info.Status = msg.NewStatus
		info.OperatorFee = msg.NewOperatorFee
		info.MinOperatorShareRate = msg.NewMinOperatorShareRate
		return nil
	}); err != nil {
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
		types.EventTypeUpdatePublicPool,
		sdk.NewAttribute(types.AttributeKeyPoolAccountIndex, strconv.FormatUint(pool.AccountIndex, 10)),
		sdk.NewAttribute(types.AttributeKeyStatus, strconv.FormatUint(uint64(msg.NewStatus), 10)),
		sdk.NewAttribute(types.AttributeKeyOperatorFee, strconv.FormatUint(uint64(msg.NewOperatorFee), 10)),
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
	var firstNonZero *types.AccountPosition
	if err := m.IterateAccountPositions(ctx, pool.AccountIndex, func(p types.AccountPosition) bool {
		if !p.BaseSize.IsZero() {
			pos := p
			firstNonZero = &pos
			return true
		}
		return false
	}); err != nil {
		return err
	}
	if firstNonZero != nil {
		return types.ErrPoolMustBeEmpty.Wrapf(
			"pool has non-zero position in market %d", firstNonZero.MarketIndex,
		)
	}
	return nil
}

// ---------- helpers shared by mint/burn ----------

// resolveSenderMaster returns sender's master account, used by mint/burn
// to identify the LP account row for the share entry.
func (m msgServer) resolveSenderMaster(ctx context.Context, sender string) (types.Account, error) {
	master, err := m.GetMasterAccountByOwner(ctx, sender)
	if err != nil {
		return types.Account{}, types.ErrAccountNotFound.Wrapf("sender has no perpdex account: %s", err.Error())
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
// allocates fresh shares at the current NAV. Principal is the cumulative
// cost-basis used by future burn profit calc; entry_timestamp is reset
// on every non-operator mint and gates the LLP burn cooldown.
func (m msgServer) MintShares(ctx context.Context, msg *types.MsgMintShares) (*types.MsgMintSharesResponse, error) {
	if err := msg.ValidateBasic(); err != nil {
		return nil, err
	}
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

	// Minting drains master collateral, so settle pending funding and
	// snapshot pre-state risk before mutating. Without this, a master
	// with open positions could mint into the pool and let liquidation
	// recover the difference at the system's expense.
	if err := m.settleAllPositionFunding(ctx, master.AccountIndex); err != nil {
		return nil, err
	}
	pre, err := m.snapshotPreRisk(ctx, master.AccountIndex)
	if err != nil {
		return nil, err
	}

	// Move funds: master.collateral -> pool.collateral.
	if err := m.AddCollateral(ctx, master.AccountIndex, collatDelta.Neg()); err != nil {
		return nil, err
	}
	if err := m.AddCollateral(ctx, pool.AccountIndex, collatDelta); err != nil {
		return nil, err
	}

	if _, err := m.UpdatePublicPoolInfo(ctx, pool.AccountIndex, func(info *types.PublicPoolInfo) error {
		info.TotalShares = info.TotalShares.Add(shareAmount)
		if isOperator {
			info.OperatorShares = info.OperatorShares.Add(shareAmount)
			return nil
		}
		// Non-operator mint may not break min_operator_share_rate.
		if !CheckMinOperatorShareRate(*info) {
			return types.ErrOperatorRateViolation
		}
		return nil
	}); err != nil {
		return nil, err
	}

	if !isOperator {
		now := sdk.UnwrapSDKContext(ctx).BlockTime().UnixMilli()
		if err := m.UpsertPublicPoolShare(ctx, master.AccountIndex, pool.AccountIndex, shareAmount, usdc, now); err != nil {
			return nil, err
		}
	}

	if err := m.requireRiskOKFrom(ctx, master.AccountIndex, pre); err != nil {
		return nil, err
	}

	sdk.UnwrapSDKContext(ctx).EventManager().EmitEvent(sdk.NewEvent(
		types.EventTypeMintShares,
		sdk.NewAttribute(types.AttributeKeyPoolAccountIndex, strconv.FormatUint(pool.AccountIndex, 10)),
		sdk.NewAttribute(types.AttributeKeySenderMaster, strconv.FormatUint(master.AccountIndex, 10)),
		sdk.NewAttribute(types.AttributeKeyShareAmount, shareAmount.String()),
		sdk.NewAttribute(types.AttributeKeyPrincipalAmount, usdc.String()),
	))
	return &types.MsgMintSharesResponse{ShareAmount: shareAmount}, nil
}

// ---------- BurnShares ----------

// BurnShares redeems sender's shares from the pool back to USDC.
// Cooldown applies to LLP + non-operator burns. operator_fee_share
// is split out from realised profit and credited to operator_shares.
func (m msgServer) BurnShares(ctx context.Context, msg *types.MsgBurnShares) (*types.MsgBurnSharesResponse, error) {
	if err := msg.ValidateBasic(); err != nil {
		return nil, err
	}
	pool, err := m.GetAccount(ctx, msg.PoolAccountIndex)
	if err != nil {
		return nil, err
	}
	if !IsPoolAccount(pool) {
		return nil, types.ErrInvalidPoolAccount
	}
	depositor, err := m.resolveSenderMaster(ctx, msg.Sender)
	if err != nil {
		return nil, err
	}
	isOperator, err := m.isPoolOperator(ctx, pool, msg.Sender)
	if err != nil {
		return nil, err
	}
	return m.burnSharesCore(ctx, pool, depositor, isOperator, msg.ShareAmount, false /* skipCooldown */)
}

// burnSharesCore is the shared engine behind BurnShares and
// ForceBurnShares. Callers resolve the pool / depositor / operator
// flag up front so this function can stay focused on the math and
// state transitions.
//
//	skipCooldown -> bypass LLP cooldown (force-burn path)
func (m msgServer) burnSharesCore(
	ctx context.Context,
	pool types.Account,
	depositor types.Account,
	isOperator bool,
	shareAmount math.Int,
	skipCooldown bool,
) (*types.MsgBurnSharesResponse, error) {
	if !IsPoolAccount(pool) {
		return nil, types.ErrInvalidPoolAccount
	}
	info := pool.PublicPoolInfo
	// Status gate: ACTIVE and FROZEN both allow burn (frozen pools must
	// continue to honour LP exits). Any unknown/future state is rejected.
	if !BurnAllowed(*info) {
		return nil, types.ErrPoolNotActive.Wrapf("status=%d", info.Status)
	}
	poolIdx := pool.AccountIndex

	// Pool must be healthy enough to burn (non-liquidation cross risk).
	// We approximate by requiring TAV > 0, which AvailableSharesToBurn
	// already enforces.
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

	// Realised profit + operator fee math (applies only to non-operator burns).
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

	// Delivered USDC scales linearly with burnedShares relative to
	// the originally-quoted shareAmount, so we avoid a second
	// SharesToUSDCValue / TAV roundtrip:
	//   delivered = usdcValue * burnedShares / shareAmount
	// shareAmount is guaranteed positive (msg validation rejects 0
	// and the cap check above requires shareAmount <= available).
	deliveredUSDC := usdcValue.Mul(burnedShares).Quo(shareAmount)
	deliveredCollat := deliveredUSDC.Mul(math.NewIntFromUint64(perptypes.USDCToCollateralMultiplier))

	// Mutate pool.public_pool_info via the cohesive helper. The
	// callback runs before persistence so the operator-floor check
	// can short-circuit the write on violation.
	if _, err := m.UpdatePublicPoolInfo(ctx, poolIdx, func(info *types.PublicPoolInfo) error {
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
			return types.ErrOperatorRateViolation
		}
		return nil
	}); err != nil {
		return nil, err
	}

	if err := m.AddCollateral(ctx, poolIdx, deliveredCollat.Neg()); err != nil {
		return nil, err
	}
	if err := m.AddCollateral(ctx, depositor.AccountIndex, deliveredCollat); err != nil {
		return nil, err
	}

	if !isOperator {
		if err := m.ReducePublicPoolShare(ctx, depositor.AccountIndex, poolIdx, shareAmount); err != nil {
			return nil, err
		}
	}

	sdk.UnwrapSDKContext(ctx).EventManager().EmitEvent(sdk.NewEvent(
		types.EventTypeBurnShares,
		sdk.NewAttribute(types.AttributeKeyPoolAccountIndex, strconv.FormatUint(poolIdx, 10)),
		sdk.NewAttribute(types.AttributeKeyDepositor, strconv.FormatUint(depositor.AccountIndex, 10)),
		sdk.NewAttribute(types.AttributeKeyShareAmount, shareAmount.String()),
		sdk.NewAttribute(types.AttributeKeyOperatorFeeShares, operatorFeeShares.String()),
		sdk.NewAttribute(types.AttributeKeyUsdcAmount, deliveredUSDC.String()),
	))
	return &types.MsgBurnSharesResponse{UsdcAmount: deliveredUSDC.Uint64()}, nil
}

// ---------- StrategyTransfer ----------

// StrategyTransfer reallocates collateral between IF strategy buckets.
// Pool must be IF, non-frozen, sender must be the pool operator,
// from != to, and the from bucket must hold the requested amount.
func (m msgServer) StrategyTransfer(ctx context.Context, msg *types.MsgStrategyTransfer) (*types.MsgStrategyTransferResponse, error) {
	if err := msg.ValidateBasic(); err != nil {
		return nil, err
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
	if err := m.assertPoolOperator(ctx, pool, msg.Sender); err != nil {
		return nil, err
	}
	if err := EnsureNotFrozen(pool.PublicPoolInfo); err != nil {
		return nil, err
	}
	// from/to strategy bounds + (from != to) are enforced by ValidateBasic.
	if _, err := m.UpdatePublicPoolInfo(ctx, pool.AccountIndex, func(info *types.PublicPoolInfo) error {
		if len(info.Strategies) != perptypes.NbStrategies {
			// Defensive: rebuild zeros if migration ever drifts the slot count.
			fixed := make([]math.Int, perptypes.NbStrategies)
			for i := range fixed {
				fixed[i] = math.ZeroInt()
			}
			copy(fixed, info.Strategies)
			info.Strategies = fixed
		}
		from := info.Strategies[msg.FromStrategy]
		if from.LT(msg.Amount) {
			return types.ErrInsufficientFunds.Wrapf(
				"strategy[%d] has %s, need %s",
				msg.FromStrategy, from.String(), msg.Amount.String(),
			)
		}
		to := info.Strategies[msg.ToStrategy]
		info.Strategies[msg.FromStrategy] = from.Sub(msg.Amount)
		info.Strategies[msg.ToStrategy] = to.Add(msg.Amount)
		return nil
	}); err != nil {
		return nil, err
	}

	sdk.UnwrapSDKContext(ctx).EventManager().EmitEvent(sdk.NewEvent(
		types.EventTypeStrategyTransfer,
		sdk.NewAttribute(types.AttributeKeyPoolAccountIndex, strconv.FormatUint(pool.AccountIndex, 10)),
		sdk.NewAttribute(types.AttributeKeyFrom, strconv.FormatUint(uint64(msg.FromStrategy), 10)),
		sdk.NewAttribute(types.AttributeKeyTo, strconv.FormatUint(uint64(msg.ToStrategy), 10)),
		sdk.NewAttribute(types.AttributeKeyAmount, msg.Amount.String()),
	))
	return &types.MsgStrategyTransferResponse{}, nil
}

// ---------- ForceBurnShares ----------

// ForceBurnShares is the gov-authority escape hatch on the LLP pool.
// Bypasses the cooldown, otherwise mirrors BurnShares.
func (m msgServer) ForceBurnShares(ctx context.Context, msg *types.MsgForceBurnShares) (*types.MsgForceBurnSharesResponse, error) {
	if err := msg.ValidateBasic(); err != nil {
		return nil, err
	}
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

	// Force-burn unwinds an LP entry, so the depositor must be a real
	// master account. ForceBurn never targets the operator path (gov
	// authority is not the pool operator at the IF), so isOperator=false.
	depositor, err := m.GetAccount(ctx, msg.DepositorAccountIndex)
	if err != nil {
		return nil, types.ErrInvalidDepositorIndex.Wrapf("%s", err.Error())
	}
	resp, err := m.burnSharesCore(ctx, pool, depositor, false /* isOperator */, msg.ShareAmount, true /* skipCooldown */)
	if err != nil {
		return nil, err
	}
	return &types.MsgForceBurnSharesResponse{UsdcAmount: resp.UsdcAmount}, nil
}
