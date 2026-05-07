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

// Deposit converts cosmos coins from the sender into perpdex collateral or
// spot balance. If the beneficiary has no master account yet, one is created
// automatically.
func (m msgServer) Deposit(ctx context.Context, msg *types.MsgDeposit) (*types.MsgDepositResponse, error) {
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

	// Route specific guards.
	if msg.RouteType == perptypes.RouteTypePerps && asset.MarginMode != perptypes.MarginModeEnabled {
		return nil, types.ErrAssetNotMargin
	}
	if msg.RouteType != perptypes.RouteTypePerps && msg.RouteType != perptypes.RouteTypeSpot {
		return nil, types.ErrInvalidRoute
	}

	if msg.Amount < asset.MinTransferAmount {
		return nil, types.ErrAmountTooSmall.Wrapf("amount=%d min=%d", msg.Amount, asset.MinTransferAmount)
	}

	// Pull the coin into the module account.
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
	if msg.Amount < asset.MinWithdrawalAmount {
		return nil, types.ErrAmountTooSmall
	}
	// Settle pending funding on all non-zero positions so the post-state
	// risk check sees the up-to-date collateral/entry_quote.
	if err := m.settleAllPositionFunding(ctx, msg.AccountIndex); err != nil {
		return nil, err
	}
	// Capture pre-state risk so IsValidRiskChange can enforce the
	// "strict improvement" rule for accounts that are already
	// unhealthy (e.g. returning collateral to a HEALTHY state).
	if err := m.snapshotPreRisk(ctx, msg.AccountIndex); err != nil {
		return nil, err
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
		if err := m.AddCollateral(ctx, msg.AccountIndex, delta.Neg()); err != nil {
			return nil, err
		}
	case perptypes.RouteTypeSpot:
		if err := m.AddAccountAssetBalance(ctx, msg.AccountIndex, msg.AssetIndex, delta.Neg()); err != nil {
			return nil, err
		}
	default:
		return nil, types.ErrInvalidRoute
	}

	if err := m.requireRiskOK(ctx, msg.AccountIndex); err != nil {
		return nil, err
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
	return &types.MsgWithdrawResponse{}, nil
}

func (m msgServer) CreateSubAccount(ctx context.Context, msg *types.MsgCreateSubAccount) (*types.MsgCreateSubAccountResponse, error) {
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
	return &types.MsgCreateSubAccountResponse{SubAccountIndex: sub.AccountIndex}, nil
}

func (m msgServer) UpdateAccountConfig(ctx context.Context, msg *types.MsgUpdateAccountConfig) (*types.MsgUpdateAccountConfigResponse, error) {
	if msg.NewTradingMode != perptypes.AccountTradingModeSimple && msg.NewTradingMode != perptypes.AccountTradingModeUnified {
		return nil, types.ErrInvalidTradingMode
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
	return &types.MsgUpdateAccountConfigResponse{}, nil
}

func (m msgServer) UpdateAccountAssetConfig(ctx context.Context, msg *types.MsgUpdateAccountAssetConfig) (*types.MsgUpdateAccountAssetConfigResponse, error) {
	if msg.NewMarginMode != perptypes.MarginModeDisabled &&
		msg.NewMarginMode != perptypes.MarginModeEnabled {
		return nil, types.ErrInvalidMarginMode.Wrapf("new_margin_mode=%d", msg.NewMarginMode)
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
	return &types.MsgUpdateAccountAssetConfigResponse{}, nil
}

func (m msgServer) Transfer(ctx context.Context, msg *types.MsgTransfer) (*types.MsgTransferResponse, error) {
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
	// Settle pending funding on the sender (the account whose risk we check)
	// so the post-state risk uses the up-to-date collateral / entry_quote.
	if err := m.settleAllPositionFunding(ctx, msg.FromAccountIndex); err != nil {
		return nil, err
	}
	if err := m.snapshotPreRisk(ctx, msg.FromAccountIndex); err != nil {
		return nil, err
	}
	delta := math.NewIntFromUint64(msg.Amount).Mul(math.NewIntFromUint64(asset.ExtensionMultiplier))

	// We move USDC-style collateral when the asset is margin-enabled, else the
	// spot balance row.
	if asset.MarginMode == perptypes.MarginModeEnabled {
		if err := m.AddCollateral(ctx, msg.FromAccountIndex, delta.Neg()); err != nil {
			return nil, err
		}
		if err := m.AddCollateral(ctx, msg.ToAccountIndex, delta); err != nil {
			return nil, err
		}
	} else {
		if err := m.AddAccountAssetBalance(ctx, msg.FromAccountIndex, msg.AssetIndex, delta.Neg()); err != nil {
			return nil, err
		}
		if err := m.AddAccountAssetBalance(ctx, msg.ToAccountIndex, msg.AssetIndex, delta); err != nil {
			return nil, err
		}
	}

	if err := m.requireRiskOK(ctx, msg.FromAccountIndex); err != nil {
		return nil, err
	}
	return &types.MsgTransferResponse{}, nil
}

func (m msgServer) UpdateMargin(ctx context.Context, msg *types.MsgUpdateMargin) (*types.MsgUpdateMarginResponse, error) {
	if ok, err := m.IsAuthorized(ctx, msg.Sender, msg.AccountIndex); err != nil {
		return nil, err
	} else if !ok {
		return nil, types.ErrUnauthorized
	}
	if err := m.rejectPoolAccount(ctx, msg.AccountIndex); err != nil {
		return nil, err
	}
	if msg.Action != perptypes.AddMargin && msg.Action != perptypes.RemoveMargin {
		return nil, types.ErrInvalidMarginAction
	}
	// Settle pending funding on the touched isolated position so its
	// allocated_margin/entry_quote/collateral reflect the latest rate
	// before the risk check fires.
	if err := m.fundingKeeper.SettlePositionFunding(ctx, msg.AccountIndex, msg.MarketIndex); err != nil {
		return nil, err
	}
	if err := m.snapshotPreRisk(ctx, msg.AccountIndex); err != nil {
		return nil, err
	}
	pos, err := m.GetPosition(ctx, msg.AccountIndex, msg.MarketIndex)
	if err != nil {
		return nil, err
	}
	if pos.MarginMode != perptypes.IsolatedMargin {
		return nil, types.ErrPositionNotIsolated
	}
	if msg.Amount.IsNil() {
		return nil, types.ErrInvalidParams.Wrap("amount must be set")
	}
	amount := msg.Amount

	// AllocatedMargin is already normalised to ZeroInt() by
	// GetPosition / UpdatePosition's auto-vivified default, so the
	// inline IsNil-guards used to live here are redundant.
	switch msg.Action {
	case perptypes.AddMargin:
		if _, err := m.UpdatePosition(ctx, msg.AccountIndex, msg.MarketIndex, func(p *types.AccountPosition) error {
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
		if _, err := m.UpdatePosition(ctx, msg.AccountIndex, msg.MarketIndex, func(p *types.AccountPosition) error {
			p.AllocatedMargin = p.AllocatedMargin.Sub(amount)
			return nil
		}); err != nil {
			return nil, err
		}
		if err := m.AddCollateral(ctx, msg.AccountIndex, amount); err != nil {
			return nil, err
		}
	}

	if err := m.requireRiskOK(ctx, msg.AccountIndex); err != nil {
		return nil, err
	}
	return &types.MsgUpdateMarginResponse{}, nil
}

func (m msgServer) UpdateLeverage(ctx context.Context, msg *types.MsgUpdateLeverage) (*types.MsgUpdateLeverageResponse, error) {
	if ok, err := m.IsAuthorized(ctx, msg.Sender, msg.AccountIndex); err != nil {
		return nil, err
	} else if !ok {
		return nil, types.ErrUnauthorized
	}
	if err := m.rejectPoolAccount(ctx, msg.AccountIndex); err != nil {
		return nil, err
	}
	if msg.NewMarginMode != perptypes.CrossMargin && msg.NewMarginMode != perptypes.IsolatedMargin {
		return nil, types.ErrInvalidMarginMode.Wrapf("new_margin_mode=%d", msg.NewMarginMode)
	}
	// Market + IMF validation: the market must exist, and the new IMF
	// must satisfy market_min <= new_imf <= margin_tick.
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
	if msg.NewInitialMarginFraction > uint32(perptypes.MarginTick) {
		return nil, types.ErrInvalidParams.Wrapf(
			"new_initial_margin_fraction=%d exceeds MarginTick=%d",
			msg.NewInitialMarginFraction, perptypes.MarginTick,
		)
	}
	pos, err := m.GetPosition(ctx, msg.AccountIndex, msg.MarketIndex)
	if err != nil {
		return nil, err
	}
	if !pos.Position.IsZero() {
		return nil, types.ErrPositionNotEmpty.Wrap("must close position before updating leverage/margin mode")
	}
	if err := m.SetPositionLeverage(ctx, msg.AccountIndex, msg.MarketIndex, msg.NewMarginMode, msg.NewInitialMarginFraction); err != nil {
		return nil, err
	}
	return &types.MsgUpdateLeverageResponse{}, nil
}

func (m msgServer) UpdateParams(ctx context.Context, msg *types.MsgUpdateParams) (*types.MsgUpdateParamsResponse, error) {
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
