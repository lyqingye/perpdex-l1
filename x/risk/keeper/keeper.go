package keeper

import (
	"context"
	"errors"
	"fmt"

	"cosmossdk.io/collections"
	"cosmossdk.io/core/store"
	"cosmossdk.io/math"

	"github.com/cosmos/cosmos-sdk/codec"

	perptypes "github.com/perpdex/perpdex-l1/types"
	"github.com/perpdex/perpdex-l1/x/risk/types"
)

// Keeper implements the pure risk computations described in 16-risk.md. It
// owns no state outside of the pre-computed cache used to short-circuit
// IsValidRiskChange across handler invocations.
type Keeper struct {
	cdc           codec.BinaryCodec
	storeService  store.KVStoreService
	authority     string
	accountKeeper types.AccountKeeper
	marketKeeper  types.MarketKeeper
	oracleKeeper  types.OracleKeeper

	Schema collections.Schema
	Params collections.Item[types.Params]
	// Cache holds the pre-state risk parameters for an account during a
	// transaction so the post-state can be compared against it efficiently.
	Cache collections.Map[uint64, types.RiskParameters]
}

func NewKeeper(cdc codec.BinaryCodec, storeService store.KVStoreService, authority string,
	ak types.AccountKeeper, mk types.MarketKeeper, ok types.OracleKeeper,
) Keeper {
	sb := collections.NewSchemaBuilder(storeService)
	k := Keeper{
		cdc:           cdc,
		storeService:  storeService,
		authority:     authority,
		accountKeeper: ak,
		marketKeeper:  mk,
		oracleKeeper:  ok,

		Params: collections.NewItem(sb, types.ParamsKey, "params", codec.CollValue[types.Params](cdc)),
		Cache:  collections.NewMap(sb, []byte{0x01}, "cache", collections.Uint64Key, codec.CollValue[types.RiskParameters](cdc)),
	}
	schema, err := sb.Build()
	if err != nil {
		panic(fmt.Errorf("risk: %w", err))
	}
	k.Schema = schema
	return k
}

func (k Keeper) Authority() string { return k.authority }

// ComputeRiskInfo iterates all positions of an account and aggregates their
// risk contributions into a RiskInfo struct. cross_risk_parameters considers
// only cross-margined positions; current_risk_parameters considers all.
func (k Keeper) ComputeRiskInfo(ctx context.Context, accountIdx uint64) (types.RiskInfo, error) {
	a, err := k.accountKeeper.GetAccount(ctx, accountIdx)
	if err != nil {
		return types.RiskInfo{}, err
	}
	collateral := a.Collateral
	if collateral.IsNil() {
		collateral = math.ZeroInt()
	}

	cross := types.RiskParameters{
		Collateral:                   collateral,
		CollateralWithFunding:        collateral,
		TotalAccountValue:            collateral,
		InitialMarginRequirement:     math.ZeroInt(),
		MaintenanceMarginRequirement: math.ZeroInt(),
		CloseOutMarginRequirement:    math.ZeroInt(),
	}
	current := cross

	imSum := math.ZeroInt()
	mmSum := math.ZeroInt()
	cmSum := math.ZeroInt()
	totalCross := collateral
	totalAll := collateral

	for marketIdx := uint32(0); marketIdx <= perptypes.MaxPerpsMarketIndex; marketIdx++ {
		pos, err := k.accountKeeper.GetPosition(ctx, accountIdx, marketIdx)
		if err != nil {
			continue
		}
		if pos.Position.IsZero() {
			continue
		}
		px, err := k.oracleKeeper.GetPrice(ctx, marketIdx)
		if err != nil {
			continue
		}
		md, err := k.marketKeeper.GetMarketDetails(ctx, marketIdx)
		if err != nil {
			continue
		}
		notional := pos.Position.Abs().Mul(math.NewIntFromUint64(uint64(px.MarkPrice)))
		// Margin requirements scale by basis points (precision 1e4).
		im := notional.Mul(math.NewIntFromUint64(uint64(md.DefaultInitialMarginFraction))).Quo(math.NewInt(int64(perptypes.MarginTick)))
		mm := notional.Mul(math.NewIntFromUint64(uint64(md.MaintenanceMarginFraction))).Quo(math.NewInt(int64(perptypes.MarginTick)))
		cm := notional.Mul(math.NewIntFromUint64(uint64(md.CloseOutMarginFraction))).Quo(math.NewInt(int64(perptypes.MarginTick)))
		// Unrealized PnL: position * (mark - entry_price). Approximate entry
		// price by entry_quote / position when available.
		uPnL := pos.Position.Mul(math.NewIntFromUint64(uint64(px.MarkPrice))).Sub(pos.EntryQuote)

		if pos.MarginMode == perptypes.IsolatedMargin {
			totalAll = totalAll.Add(pos.AllocatedMargin).Add(uPnL)
		} else {
			imSum = imSum.Add(im)
			mmSum = mmSum.Add(mm)
			cmSum = cmSum.Add(cm)
			totalCross = totalCross.Add(uPnL)
			totalAll = totalAll.Add(uPnL)
		}
	}

	cross.TotalAccountValue = totalCross
	cross.InitialMarginRequirement = imSum
	cross.MaintenanceMarginRequirement = mmSum
	cross.CloseOutMarginRequirement = cmSum

	current.TotalAccountValue = totalAll
	current.InitialMarginRequirement = imSum
	current.MaintenanceMarginRequirement = mmSum
	current.CloseOutMarginRequirement = cmSum

	return types.RiskInfo{CrossRiskParameters: &cross, CurrentRiskParameters: &current}, nil
}

// ComputeIsolatedRisk returns risk parameters for one isolated position.
func (k Keeper) ComputeIsolatedRisk(ctx context.Context, accountIdx uint64, marketIdx uint32) (types.RiskParameters, error) {
	pos, err := k.accountKeeper.GetPosition(ctx, accountIdx, marketIdx)
	if err != nil {
		return types.RiskParameters{}, err
	}
	if pos.MarginMode != perptypes.IsolatedMargin {
		return types.RiskParameters{}, fmt.Errorf("position is not isolated")
	}
	px, err := k.oracleKeeper.GetPrice(ctx, marketIdx)
	if err != nil {
		return types.RiskParameters{}, err
	}
	md, err := k.marketKeeper.GetMarketDetails(ctx, marketIdx)
	if err != nil {
		return types.RiskParameters{}, err
	}
	notional := pos.Position.Abs().Mul(math.NewIntFromUint64(uint64(px.MarkPrice)))
	im := notional.Mul(math.NewIntFromUint64(uint64(md.DefaultInitialMarginFraction))).Quo(math.NewInt(int64(perptypes.MarginTick)))
	mm := notional.Mul(math.NewIntFromUint64(uint64(md.MaintenanceMarginFraction))).Quo(math.NewInt(int64(perptypes.MarginTick)))
	cm := notional.Mul(math.NewIntFromUint64(uint64(md.CloseOutMarginFraction))).Quo(math.NewInt(int64(perptypes.MarginTick)))
	uPnL := pos.Position.Mul(math.NewIntFromUint64(uint64(px.MarkPrice))).Sub(pos.EntryQuote)
	tav := pos.AllocatedMargin.Add(uPnL)
	return types.RiskParameters{
		Collateral:                   pos.AllocatedMargin,
		CollateralWithFunding:        pos.AllocatedMargin,
		TotalAccountValue:            tav,
		InitialMarginRequirement:     im,
		MaintenanceMarginRequirement: mm,
		CloseOutMarginRequirement:    cm,
	}, nil
}

// GetHealthStatus maps the current risk parameters to one of the 5 levels.
func (k Keeper) GetHealthStatus(ctx context.Context, accountIdx uint64) (uint32, error) {
	ri, err := k.ComputeRiskInfo(ctx, accountIdx)
	if err != nil {
		return 0, err
	}
	cur := ri.CurrentRiskParameters
	if cur == nil {
		return perptypes.HealthHealthy, nil
	}
	return classifyHealth(*cur), nil
}

// classifyHealth implements the 5-level state machine.
func classifyHealth(p types.RiskParameters) uint32 {
	if p.TotalAccountValue.IsNegative() {
		return perptypes.HealthBankruptcy
	}
	if p.TotalAccountValue.LT(p.CloseOutMarginRequirement) {
		return perptypes.HealthFullLiquidation
	}
	if p.TotalAccountValue.LT(p.MaintenanceMarginRequirement) {
		return perptypes.HealthPartialLiquidation
	}
	if p.TotalAccountValue.LT(p.InitialMarginRequirement) {
		return perptypes.HealthPreLiquidation
	}
	return perptypes.HealthHealthy
}

// GetAvailableCollateral returns total_account_value - initial_margin_requirement.
func (k Keeper) GetAvailableCollateral(ctx context.Context, accountIdx uint64) (math.Int, error) {
	ri, err := k.ComputeRiskInfo(ctx, accountIdx)
	if err != nil {
		return math.ZeroInt(), err
	}
	cur := ri.CurrentRiskParameters
	if cur == nil {
		return math.ZeroInt(), nil
	}
	return cur.TotalAccountValue.Sub(cur.InitialMarginRequirement), nil
}

// IsValidRiskChange returns true if the post-state risk is healthy or
// strictly improving versus the cached pre-state. When no pre-state cache is
// found we treat any healthy post-state as valid.
func (k Keeper) IsValidRiskChange(ctx context.Context, accountIdx uint64) (bool, error) {
	post, err := k.ComputeRiskInfo(ctx, accountIdx)
	if err != nil {
		return false, err
	}
	postP := post.CurrentRiskParameters
	postClass := classifyHealth(*postP)
	if postClass == perptypes.HealthHealthy {
		return true, nil
	}
	pre, err := k.Cache.Get(ctx, accountIdx)
	if err != nil {
		if errors.Is(err, collections.ErrNotFound) {
			return postClass == perptypes.HealthHealthy, nil
		}
		return false, err
	}
	preClass := classifyHealth(pre)
	// post must not be worse than pre — cross multiplication:
	// (post.tav * pre.im) >= (pre.tav * post.im) when both sides healthy-ish
	if postP.InitialMarginRequirement.IsZero() {
		return true, nil
	}
	if pre.InitialMarginRequirement.IsZero() {
		return postClass <= preClass, nil
	}
	lhs := postP.TotalAccountValue.Mul(pre.InitialMarginRequirement)
	rhs := pre.TotalAccountValue.Mul(postP.InitialMarginRequirement)
	return !lhs.LT(rhs) && postClass <= preClass, nil
}

// SnapshotPreRisk caches the pre-state RiskParameters for an account so
// IsValidRiskChange can compare after handlers run.
func (k Keeper) SnapshotPreRisk(ctx context.Context, accountIdx uint64) error {
	post, err := k.ComputeRiskInfo(ctx, accountIdx)
	if err != nil {
		return err
	}
	if post.CurrentRiskParameters == nil {
		return nil
	}
	return k.Cache.Set(ctx, accountIdx, *post.CurrentRiskParameters)
}

// GetPositionZeroPrice returns the mark price at which the position would
// have zero TAV. Approximated via entry_quote / size.
func (k Keeper) GetPositionZeroPrice(ctx context.Context, accountIdx uint64, marketIdx uint32) (uint32, error) {
	pos, err := k.accountKeeper.GetPosition(ctx, accountIdx, marketIdx)
	if err != nil {
		return 0, err
	}
	if pos.Position.IsZero() {
		return 0, nil
	}
	return uint32(pos.EntryQuote.Quo(pos.Position).Abs().Uint64()), nil
}
