package keeper

import (
	"context"
	"strconv"

	"cosmossdk.io/math"

	sdk "github.com/cosmos/cosmos-sdk/types"

	perptypes "github.com/perpdex/perpdex-l1/types"
	"github.com/perpdex/perpdex-l1/x/account/types"
)

type msgServer struct{ Keeper }

func NewMsgServerImpl(k Keeper) types.MsgServer { return &msgServer{Keeper: k} }

var _ types.MsgServer = msgServer{}

// maxUint64 returns the larger of two uint64 values. Used to combine
// asset-level and module-Params-level minimum amounts so that whichever
// floor is higher actually gates the message.
func maxUint64(a, b uint64) uint64 {
	if a > b {
		return a
	}
	return b
}

// Deposit converts cosmos coins from the sender into perpdex collateral or
// spot balance. If the beneficiary has no master account yet, one is created
// automatically.
//
// Every msg_server handler in this module starts with msg.ValidateBasic()
// as defense-in-depth: the ante handler already runs ValidateBasic for
// transactions submitted through CometBFT, but keeper-level test
// callers, governance proposals, and hypothetical cross-module callers
// can bypass ante. Re-running the stateless check here guarantees the
// invariants encoded in ValidateBasic are always enforced before any
// state is touched.
func (m msgServer) Deposit(ctx context.Context, msg *types.MsgDeposit) (*types.MsgDepositResponse, error) {
	if err := msg.ValidateBasic(); err != nil {
		return nil, err
	}
	sender, err := sdk.AccAddressFromBech32(msg.Sender)
	if err != nil {
		return nil, err
	}
	beneficiary := sender
	if msg.Beneficiary != "" {
		b, err := sdk.AccAddressFromBech32(msg.Beneficiary)
		if err != nil {
			return nil, err
		}
		beneficiary = b
	}

	asset, err := m.assetKeeper.GetAsset(ctx, msg.AssetIndex)
	if err != nil {
		return nil, err
	}
	if !asset.Enabled {
		return nil, types.ErrAssetDisabled
	}

	// Route enum membership is enforced by MsgDeposit.ValidateBasic; here
	// we only check the per-asset constraint that the perps route is
	// reserved for margin-enabled assets.
	if msg.RouteType == perptypes.RouteTypePerps && asset.MarginMode != perptypes.MarginModeEnabled {
		return nil, types.ErrAssetNotMargin
	}

	params, err := m.Params.Get(ctx)
	if err != nil {
		return nil, err
	}
	minDeposit := maxUint64(asset.MinTransferAmount, params.MinPartialTransferAmount)
	if msg.Amount < minDeposit {
		return nil, types.ErrAmountTooSmall.Wrapf("amount=%d min=%d", msg.Amount, minDeposit)
	}

	coin := sdk.NewCoin(asset.Denom, math.NewIntFromUint64(msg.Amount))
	if err := m.bankKeeper.SendCoinsFromAccountToModule(ctx, sender, types.ModuleName, sdk.NewCoins(coin)); err != nil {
		return nil, err
	}

	acc, err := m.EnsureMasterAccount(ctx, beneficiary)
	if err != nil {
		return nil, err
	}

	// Convert external precision -> internal collateral precision (multiplier).
	delta := math.NewIntFromUint64(msg.Amount).Mul(math.NewIntFromUint64(asset.ExtensionMultiplier))

	if msg.RouteType == perptypes.RouteTypePerps {
		if err := m.AddCollateral(ctx, acc.AccountIndex, delta); err != nil {
			return nil, err
		}
	} else {
		if err := m.AddAccountAssetBalance(ctx, acc.AccountIndex, msg.AssetIndex, delta); err != nil {
			return nil, err
		}
	}

	sdk.UnwrapSDKContext(ctx).EventManager().EmitEvent(sdk.NewEvent(
		types.EventTypeDeposit,
		sdk.NewAttribute(types.AttributeKeyAccountIndex, strconv.FormatUint(acc.AccountIndex, 10)),
		sdk.NewAttribute(types.AttributeKeyAssetIndex, strconv.FormatUint(uint64(msg.AssetIndex), 10)),
		sdk.NewAttribute(types.AttributeKeyAmount, strconv.FormatUint(msg.Amount, 10)),
		sdk.NewAttribute(types.AttributeKeyRoute, strconv.FormatUint(uint64(msg.RouteType), 10)),
	))

	return &types.MsgDepositResponse{AccountIndex: acc.AccountIndex}, nil
}

func (m msgServer) Withdraw(ctx context.Context, msg *types.MsgWithdraw) (*types.MsgWithdrawResponse, error) {
	if err := msg.ValidateBasic(); err != nil {
		return nil, err
	}
	if ok, err := m.IsAuthorized(ctx, msg.Sender, msg.AccountIndex); err != nil {
		return nil, err
	} else if !ok {
		return nil, types.ErrUnauthorized
	}
	// Pool/IF accounts must use share/strategy paths only.
	if err := m.rejectPoolAccount(ctx, msg.AccountIndex); err != nil {
		return nil, err
	}

	asset, err := m.assetKeeper.GetAsset(ctx, msg.AssetIndex)
	if err != nil {
		return nil, err
	}
	if !asset.Enabled {
		return nil, types.ErrAssetDisabled
	}
	params, err := m.Params.Get(ctx)
	if err != nil {
		return nil, err
	}
	minWithdraw := maxUint64(asset.MinWithdrawalAmount, params.MinPartialWithdrawAmount)
	if msg.Amount < minWithdraw {
		return nil, types.ErrAmountTooSmall.Wrapf("amount=%d min=%d", msg.Amount, minWithdraw)
	}

	// Internal precision delta to subtract.
	delta := math.NewIntFromUint64(msg.Amount).Mul(math.NewIntFromUint64(asset.ExtensionMultiplier))

	switch msg.RouteType {
	case perptypes.RouteTypePerps:
		// Perps route shares a canonical collateral bucket; only margin-enabled
		// assets can settle via the perps route, symmetrically to Deposit.
		if asset.MarginMode != perptypes.MarginModeEnabled {
			return nil, types.ErrAssetNotMargin
		}
		// Settle pending funding on all non-zero positions so the post-state
		// risk check sees the up-to-date collateral/entry_quote. Only the
		// perps route consumes/affects the collateral bucket on which risk
		// is computed; spot withdrawals never touch margin, so we skip the
		// funding sweep on that path.
		if err := m.settleAllPositionFunding(ctx, msg.AccountIndex); err != nil {
			return nil, err
		}
		// Capture pre-state risk so requireRiskOKFrom can enforce the
		// "strict improvement" rule for accounts that are already
		// unhealthy (e.g. returning collateral to a HEALTHY state).
		pre, err := m.snapshotPreRisk(ctx, msg.AccountIndex)
		if err != nil {
			return nil, err
		}
		if err := m.AddCollateral(ctx, msg.AccountIndex, delta.Neg()); err != nil {
			return nil, err
		}
		if err := m.requireRiskOKFrom(ctx, msg.AccountIndex, pre); err != nil {
			return nil, err
		}
	case perptypes.RouteTypeSpot:
		// Spot withdrawals never touch margin/positions. AddAccountAssetBalance
		// enforces Available >= |delta| so resting spot locks are honoured.
		if err := m.AddAccountAssetBalance(ctx, msg.AccountIndex, msg.AssetIndex, delta.Neg()); err != nil {
			return nil, err
		}
		// RouteType is already restricted to {Perps, Spot} by
		// MsgWithdraw.ValidateBasic, so no default branch is needed.
	}

	dest, err := sdk.AccAddressFromBech32(msg.Sender)
	if err != nil {
		return nil, err
	}
	if msg.DestinationAddress != "" {
		dest, err = sdk.AccAddressFromBech32(msg.DestinationAddress)
		if err != nil {
			return nil, err
		}
	}
	coin := sdk.NewCoin(asset.Denom, math.NewIntFromUint64(msg.Amount))
	if err := m.bankKeeper.SendCoinsFromModuleToAccount(ctx, types.ModuleName, dest, sdk.NewCoins(coin)); err != nil {
		return nil, err
	}

	sdk.UnwrapSDKContext(ctx).EventManager().EmitEvent(sdk.NewEvent(
		types.EventTypeWithdraw,
		sdk.NewAttribute(types.AttributeKeyAccountIndex, strconv.FormatUint(msg.AccountIndex, 10)),
		sdk.NewAttribute(types.AttributeKeyAssetIndex, strconv.FormatUint(uint64(msg.AssetIndex), 10)),
		sdk.NewAttribute(types.AttributeKeyAmount, strconv.FormatUint(msg.Amount, 10)),
		sdk.NewAttribute(types.AttributeKeyRoute, strconv.FormatUint(uint64(msg.RouteType), 10)),
	))
	return &types.MsgWithdrawResponse{}, nil
}

func (m msgServer) CreateSubAccount(ctx context.Context, msg *types.MsgCreateSubAccount) (*types.MsgCreateSubAccountResponse, error) {
	if err := msg.ValidateBasic(); err != nil {
		return nil, err
	}
	master, err := m.GetAccount(ctx, msg.MasterAccountIndex)
	if err != nil {
		return nil, err
	}
	if master.OwnerAddress != msg.Sender {
		return nil, types.ErrUnauthorized
	}
	// Sub-accounts can only be opened under a master; pool/IF accounts
	// don't have user-facing sub-accounts.
	if master.AccountType != perptypes.MasterAccountType {
		return nil, types.ErrInvalidAccountType.Wrap("master is not a master account")
	}
	sub, err := m.Keeper.CreateSubAccount(ctx, master)
	if err != nil {
		return nil, err
	}
	sdk.UnwrapSDKContext(ctx).EventManager().EmitEvent(sdk.NewEvent(
		types.EventTypeCreateSubAccount,
		sdk.NewAttribute(types.AttributeKeyMasterAccountIndex, strconv.FormatUint(master.AccountIndex, 10)),
		sdk.NewAttribute(types.AttributeKeySubAccountIndex, strconv.FormatUint(sub.AccountIndex, 10)),
	))
	return &types.MsgCreateSubAccountResponse{SubAccountIndex: sub.AccountIndex}, nil
}

func (m msgServer) UpdateAccountConfig(ctx context.Context, msg *types.MsgUpdateAccountConfig) (*types.MsgUpdateAccountConfigResponse, error) {
	if err := msg.ValidateBasic(); err != nil {
		return nil, err
	}
	a, err := m.GetAccount(ctx, msg.AccountIndex)
	if err != nil {
		return nil, err
	}
	if a.OwnerAddress != msg.Sender {
		return nil, types.ErrUnauthorized
	}
	if a.IsPoolType() {
		return nil, types.ErrPoolGenericMsg.Wrapf("account %d is a pool account", a.AccountIndex)
	}
	if err := m.UpdateAccountTradingMode(ctx, a.AccountIndex, msg.NewTradingMode); err != nil {
		return nil, err
	}
	sdk.UnwrapSDKContext(ctx).EventManager().EmitEvent(sdk.NewEvent(
		types.EventTypeUpdateAccountConfig,
		sdk.NewAttribute(types.AttributeKeyAccountIndex, strconv.FormatUint(a.AccountIndex, 10)),
		sdk.NewAttribute(types.AttributeKeyTradingMode, strconv.FormatUint(uint64(msg.NewTradingMode), 10)),
	))
	return &types.MsgUpdateAccountConfigResponse{}, nil
}

func (m msgServer) UpdateAccountAssetConfig(ctx context.Context, msg *types.MsgUpdateAccountAssetConfig) (*types.MsgUpdateAccountAssetConfigResponse, error) {
	if err := msg.ValidateBasic(); err != nil {
		return nil, err
	}
	a, err := m.GetAccount(ctx, msg.AccountIndex)
	if err != nil {
		return nil, err
	}
	if a.OwnerAddress != msg.Sender {
		return nil, types.ErrUnauthorized
	}
	if a.IsPoolType() {
		return nil, types.ErrPoolGenericMsg.Wrapf("account %d is a pool account", a.AccountIndex)
	}
	if err := m.SetAccountAssetMarginMode(ctx, msg.AccountIndex, msg.AssetIndex, msg.NewMarginMode); err != nil {
		return nil, err
	}
	sdk.UnwrapSDKContext(ctx).EventManager().EmitEvent(sdk.NewEvent(
		types.EventTypeUpdateAccountAssetConfig,
		sdk.NewAttribute(types.AttributeKeyAccountIndex, strconv.FormatUint(a.AccountIndex, 10)),
		sdk.NewAttribute(types.AttributeKeyAssetIndex, strconv.FormatUint(uint64(msg.AssetIndex), 10)),
		sdk.NewAttribute(types.AttributeKeyMarginMode, strconv.FormatUint(uint64(msg.NewMarginMode), 10)),
	))
	return &types.MsgUpdateAccountAssetConfigResponse{}, nil
}

func (m msgServer) Transfer(ctx context.Context, msg *types.MsgTransfer) (*types.MsgTransferResponse, error) {
	if err := msg.ValidateBasic(); err != nil {
		return nil, err
	}
	if ok, err := m.IsAuthorized(ctx, msg.Sender, msg.FromAccountIndex); err != nil {
		return nil, err
	} else if !ok {
		return nil, types.ErrUnauthorized
	}
	// Pool/IF accounts cannot be the source or destination of a
	// generic Transfer; use share/strategy paths.
	if err := m.rejectPoolAccount(ctx, msg.FromAccountIndex); err != nil {
		return nil, err
	}
	if err := m.rejectPoolAccount(ctx, msg.ToAccountIndex); err != nil {
		return nil, err
	}

	asset, err := m.assetKeeper.GetAsset(ctx, msg.AssetIndex)
	if err != nil {
		return nil, err
	}
	if !asset.Enabled {
		return nil, types.ErrAssetDisabled
	}
	params, err := m.Params.Get(ctx)
	if err != nil {
		return nil, err
	}
	minTransfer := maxUint64(asset.MinTransferAmount, params.MinPartialTransferAmount)
	if msg.Amount < minTransfer {
		return nil, types.ErrAmountTooSmall.Wrapf("amount=%d min=%d", msg.Amount, minTransfer)
	}
	delta := math.NewIntFromUint64(msg.Amount).Mul(math.NewIntFromUint64(asset.ExtensionMultiplier))

	// We move USDC-style collateral when the asset is margin-enabled, else the
	// spot balance row.
	if asset.MarginMode == perptypes.MarginModeEnabled {
		// Margin-enabled assets settle through the collateral bucket so the
		// transfer can change perp risk on the sender. Settle pending
		// funding + snapshot pre-risk before mutating collateral.
		if err := m.settleAllPositionFunding(ctx, msg.FromAccountIndex); err != nil {
			return nil, err
		}
		pre, err := m.snapshotPreRisk(ctx, msg.FromAccountIndex)
		if err != nil {
			return nil, err
		}
		if err := m.AddCollateral(ctx, msg.FromAccountIndex, delta.Neg()); err != nil {
			return nil, err
		}
		if err := m.AddCollateral(ctx, msg.ToAccountIndex, delta); err != nil {
			return nil, err
		}
		if err := m.requireRiskOKFrom(ctx, msg.FromAccountIndex, pre); err != nil {
			return nil, err
		}
	} else {
		// Spot-only assets never affect perp risk; skip the funding /
		// risk sweep. AddAccountAssetBalance enforces Available >= |delta|
		// so resting locks remain honoured.
		if err := m.AddAccountAssetBalance(ctx, msg.FromAccountIndex, msg.AssetIndex, delta.Neg()); err != nil {
			return nil, err
		}
		if err := m.AddAccountAssetBalance(ctx, msg.ToAccountIndex, msg.AssetIndex, delta); err != nil {
			return nil, err
		}
	}

	sdk.UnwrapSDKContext(ctx).EventManager().EmitEvent(sdk.NewEvent(
		types.EventTypeTransfer,
		sdk.NewAttribute(types.AttributeKeyFromAccountIndex, strconv.FormatUint(msg.FromAccountIndex, 10)),
		sdk.NewAttribute(types.AttributeKeyToAccountIndex, strconv.FormatUint(msg.ToAccountIndex, 10)),
		sdk.NewAttribute(types.AttributeKeyAssetIndex, strconv.FormatUint(uint64(msg.AssetIndex), 10)),
		sdk.NewAttribute(types.AttributeKeyAmount, strconv.FormatUint(msg.Amount, 10)),
	))
	return &types.MsgTransferResponse{}, nil
}

func (m msgServer) UpdateMargin(ctx context.Context, msg *types.MsgUpdateMargin) (*types.MsgUpdateMarginResponse, error) {
	if err := msg.ValidateBasic(); err != nil {
		return nil, err
	}
	if ok, err := m.IsAuthorized(ctx, msg.Sender, msg.AccountIndex); err != nil {
		return nil, err
	} else if !ok {
		return nil, types.ErrUnauthorized
	}
	if err := m.rejectPoolAccount(ctx, msg.AccountIndex); err != nil {
		return nil, err
	}
	// Action enum + amount.IsNil/positive are guarded by ValidateBasic.
	// Settle pending funding on the touched isolated position so its
	// allocated_margin/entry_quote/collateral reflect the latest rate
	// before the risk check fires.
	if err := m.fundingKeeper.SettlePositionFunding(ctx, msg.AccountIndex, msg.MarketIndex); err != nil {
		return nil, err
	}
	pre, err := m.snapshotPreRisk(ctx, msg.AccountIndex)
	if err != nil {
		return nil, err
	}
	pos, err := m.GetPosition(ctx, msg.AccountIndex, msg.MarketIndex)
	if err != nil {
		return nil, err
	}
	if pos.MarginMode != perptypes.IsolatedMargin {
		return nil, types.ErrPositionNotIsolated
	}
	// UpdateMargin only applies to an open position: adding margin to
	// (or pulling margin from) a leverage-only config row that has no
	// BaseSize would create a phantom allocated_margin balance with
	// no position to back it. Reject early so the lifecycle invariant
	// surfaces from a user-facing error rather than from a deeper
	// MutatePosition lifecycle violation.
	if pos.BaseSize.IsZero() {
		return nil, types.ErrPositionLifecycleViolation.Wrap(
			"UpdateMargin requires an open position (base_size != 0)")
	}
	amount := msg.Amount

	// UpdateMargin is only valid on OPEN isolated positions (the
	// pre-check above already enforced BaseSize != 0 + IsolatedMargin
	// + AllocatedMargin >= amount on remove), so the per-action
	// branch routes through MutatePosition.
	switch msg.Action {
	case perptypes.AddMargin:
		if _, err := m.MutatePosition(ctx, msg.AccountIndex, msg.MarketIndex, func(p *types.AccountPosition) error {
			p.AllocatedMargin = p.AllocatedMargin.Add(amount)
			return nil
		}); err != nil {
			return nil, err
		}
		if err := m.AddCollateral(ctx, msg.AccountIndex, amount.Neg()); err != nil {
			return nil, err
		}
	default: // RemoveMargin (validated above).
		if pos.AllocatedMargin.LT(amount) {
			return nil, types.ErrInsufficientFunds
		}
		if _, err := m.MutatePosition(ctx, msg.AccountIndex, msg.MarketIndex, func(p *types.AccountPosition) error {
			p.AllocatedMargin = p.AllocatedMargin.Sub(amount)
			return nil
		}); err != nil {
			return nil, err
		}
		if err := m.AddCollateral(ctx, msg.AccountIndex, amount); err != nil {
			return nil, err
		}
	}

	if err := m.requireRiskOKFrom(ctx, msg.AccountIndex, pre); err != nil {
		return nil, err
	}
	sdk.UnwrapSDKContext(ctx).EventManager().EmitEvent(sdk.NewEvent(
		types.EventTypeUpdateMargin,
		sdk.NewAttribute(types.AttributeKeyAccountIndex, strconv.FormatUint(msg.AccountIndex, 10)),
		sdk.NewAttribute(types.AttributeKeyMarketIndex, strconv.FormatUint(uint64(msg.MarketIndex), 10)),
		sdk.NewAttribute(types.AttributeKeyAction, strconv.FormatUint(uint64(msg.Action), 10)),
		sdk.NewAttribute(types.AttributeKeyAmount, amount.String()),
	))
	return &types.MsgUpdateMarginResponse{}, nil
}

func (m msgServer) UpdateLeverage(ctx context.Context, msg *types.MsgUpdateLeverage) (*types.MsgUpdateLeverageResponse, error) {
	if err := msg.ValidateBasic(); err != nil {
		return nil, err
	}
	if ok, err := m.IsAuthorized(ctx, msg.Sender, msg.AccountIndex); err != nil {
		return nil, err
	} else if !ok {
		return nil, types.ErrUnauthorized
	}
	if err := m.rejectPoolAccount(ctx, msg.AccountIndex); err != nil {
		return nil, err
	}
	// new_margin_mode enum membership + the MarginTick upper bound on
	// new_initial_margin_fraction are enforced by ValidateBasic. The
	// per-market floor still needs MarketKeeper lookup and remains here.
	md, err := m.marketKeeper.GetMarketDetails(ctx, msg.MarketIndex)
	if err != nil {
		return nil, err
	}
	if msg.NewInitialMarginFraction < md.MinInitialMarginFraction {
		return nil, types.ErrInvalidParams.Wrapf(
			"new_initial_margin_fraction=%d below market min=%d",
			msg.NewInitialMarginFraction, md.MinInitialMarginFraction,
		)
	}
	pos, err := m.GetPosition(ctx, msg.AccountIndex, msg.MarketIndex)
	if err != nil {
		return nil, err
	}
	if !pos.BaseSize.IsZero() {
		return nil, types.ErrPositionNotEmpty.Wrap("must close position before updating leverage/margin mode")
	}
	if err := m.SetPositionLeverage(ctx, msg.AccountIndex, msg.MarketIndex, msg.NewMarginMode, msg.NewInitialMarginFraction); err != nil {
		return nil, err
	}
	sdk.UnwrapSDKContext(ctx).EventManager().EmitEvent(sdk.NewEvent(
		types.EventTypeUpdateLeverage,
		sdk.NewAttribute(types.AttributeKeyAccountIndex, strconv.FormatUint(msg.AccountIndex, 10)),
		sdk.NewAttribute(types.AttributeKeyMarketIndex, strconv.FormatUint(uint64(msg.MarketIndex), 10)),
		sdk.NewAttribute(types.AttributeKeyMarginMode, strconv.FormatUint(uint64(msg.NewMarginMode), 10)),
		sdk.NewAttribute(types.AttributeKeyInitialMarginFraction, strconv.FormatUint(uint64(msg.NewInitialMarginFraction), 10)),
	))
	return &types.MsgUpdateLeverageResponse{}, nil
}

func (m msgServer) UpdateParams(ctx context.Context, msg *types.MsgUpdateParams) (*types.MsgUpdateParamsResponse, error) {
	if err := msg.ValidateBasic(); err != nil {
		return nil, err
	}
	if msg.Authority != m.authority {
		return nil, types.ErrInvalidAuthority
	}
	if err := msg.Params.Validate(); err != nil {
		return nil, err
	}
	if err := m.Params.Set(ctx, msg.Params); err != nil {
		return nil, err
	}
	return &types.MsgUpdateParamsResponse{}, nil
}
