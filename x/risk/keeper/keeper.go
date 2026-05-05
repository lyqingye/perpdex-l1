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

// Keeper implements the pure risk computations described in 16-risk.md and
// the Lighter "Liquidations & LLP" specification. It owns no state outside
// of the pre-state risk caches used to short-circuit IsValidRiskChange.
//
// Two independent caches are kept:
//
//   - Cache: cross risk parameters keyed by accountIndex.
//   - IsolatedCache: isolated risk parameters keyed by (accountIndex,
//     marketIndex). Each isolated position is a distinct sub-account from
//     a risk standpoint, so its pre-state is snapshotted separately.
type Keeper struct {
	cdc           codec.BinaryCodec
	storeService  store.KVStoreService
	authority     string
	accountKeeper types.AccountKeeper
	marketKeeper  types.MarketKeeper
	oracleKeeper  types.OracleKeeper

	Schema collections.Schema
	Params collections.Item[types.Params]
	// Cache holds the pre-state CROSS risk parameters for an account
	// during a transaction so the post-state can be compared against
	// it efficiently.
	Cache collections.Map[uint64, types.RiskParameters]
	// IsolatedCache holds the pre-state ISOLATED risk parameters for
	// each (account, market) pair so per-isolated post-state checks
	// can compare deltas independently from the cross account.
	IsolatedCache collections.Map[collections.Pair[uint64, uint32], types.RiskParameters]
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
		IsolatedCache: collections.NewMap(
			sb, []byte{0x02}, "isolated_cache",
			collections.PairKeyCodec(collections.Uint64Key, collections.Uint32Key),
			codec.CollValue[types.RiskParameters](cdc),
		),
	}
	schema, err := sb.Build()
	if err != nil {
		panic(fmt.Errorf("risk: %w", err))
	}
	k.Schema = schema
	return k
}

func (k Keeper) Authority() string { return k.authority }

// ComputeRiskInfo iterates all CROSS positions of an account and aggregates
// their risk contributions into a RiskInfo struct.
//
// Per Lighter spec, isolated positions are a separate accounting unit: the
// allocated margin of an isolated position only collateralises that
// position, and its uPnL is realised against AllocatedMargin (not the
// shared cross collateral). Including isolated AllocatedMargin/uPnL in
// the cross TAV — without also adding the corresponding IM/MM/CM — let
// an isolated profit silently inflate cross health and dodge cross
// liquidation. We therefore aggregate ONLY cross-margin positions here;
// isolated positions are evaluated individually via ComputeIsolatedRisk.
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

	imSum := math.ZeroInt()
	mmSum := math.ZeroInt()
	cmSum := math.ZeroInt()
	totalCross := collateral

	for marketIdx := uint32(0); marketIdx <= perptypes.MaxPerpsMarketIndex; marketIdx++ {
		pos, err := k.accountKeeper.GetPosition(ctx, accountIdx, marketIdx)
		if err != nil {
			return types.RiskInfo{}, err
		}
		if pos.Position.IsZero() {
			continue
		}
		// Skip isolated positions: they have an independent risk
		// envelope that ComputeIsolatedRisk evaluates on demand.
		if pos.MarginMode == perptypes.IsolatedMargin {
			continue
		}
		// For any NON-ZERO position the oracle must return a fresh,
		// non-zero mark. Silently skipping a missing price previously
		// made bankrupt accounts look healthy whenever the oracle
		// hiccupped. Fail-closed keeps the invariant "risk regression
		// cannot be hidden by an oracle outage".
		px, err := k.oracleKeeper.GetFreshPrice(ctx, marketIdx)
		if err != nil {
			return types.RiskInfo{}, types.ErrMissingPrice.Wrapf(
				"account=%d market=%d: %s", accountIdx, marketIdx, err.Error(),
			)
		}
		if px.MarkPrice == 0 {
			return types.RiskInfo{}, types.ErrZeroMarkPrice.Wrapf(
				"account=%d market=%d", accountIdx, marketIdx,
			)
		}
		md, err := k.marketKeeper.GetMarketDetails(ctx, marketIdx)
		if err != nil {
			return types.RiskInfo{}, err
		}
		notional := pos.Position.Abs().Mul(math.NewIntFromUint64(uint64(px.MarkPrice)))
		// Margin requirements scale by basis points (precision 1e4).
		im := notional.Mul(math.NewIntFromUint64(uint64(md.DefaultInitialMarginFraction))).Quo(math.NewInt(int64(perptypes.MarginTick)))
		mm := notional.Mul(math.NewIntFromUint64(uint64(md.MaintenanceMarginFraction))).Quo(math.NewInt(int64(perptypes.MarginTick)))
		cm := notional.Mul(math.NewIntFromUint64(uint64(md.CloseOutMarginFraction))).Quo(math.NewInt(int64(perptypes.MarginTick)))
		// Unrealized PnL: position * (mark - entry_price). Approximate entry
		// price by entry_quote / position when available.
		uPnL := pos.Position.Mul(math.NewIntFromUint64(uint64(px.MarkPrice))).Sub(pos.EntryQuote)

		imSum = imSum.Add(im)
		mmSum = mmSum.Add(mm)
		cmSum = cmSum.Add(cm)
		totalCross = totalCross.Add(uPnL)
	}

	cross.TotalAccountValue = totalCross
	cross.InitialMarginRequirement = imSum
	cross.MaintenanceMarginRequirement = mmSum
	cross.CloseOutMarginRequirement = cmSum

	// Both cross_risk_parameters and current_risk_parameters describe
	// the cross account. Isolated positions are queried separately via
	// ComputeIsolatedRisk / GetIsolatedHealthStatus. Returning the
	// same pointer twice would let downstream callers mutate one and
	// surprise the other; we deep-copy via two struct values.
	current := cross
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
	px, err := k.oracleKeeper.GetFreshPrice(ctx, marketIdx)
	if err != nil {
		return types.RiskParameters{}, types.ErrMissingPrice.Wrapf(
			"account=%d market=%d: %s", accountIdx, marketIdx, err.Error(),
		)
	}
	if px.MarkPrice == 0 {
		return types.RiskParameters{}, types.ErrZeroMarkPrice.Wrapf(
			"account=%d market=%d", accountIdx, marketIdx,
		)
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

// GetHealthStatus returns the CROSS health status. Isolated positions
// have their own per-market health envelope; query
// GetIsolatedHealthStatus for those.
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

// GetIsolatedHealthStatus classifies the health of one isolated
// position. Returns HealthHealthy when the position is empty or in
// cross mode (the latter is a programming error from the caller).
func (k Keeper) GetIsolatedHealthStatus(ctx context.Context, accountIdx uint64, marketIdx uint32) (uint32, error) {
	pos, err := k.accountKeeper.GetPosition(ctx, accountIdx, marketIdx)
	if err != nil {
		return 0, err
	}
	if pos.Position.IsZero() || pos.MarginMode != perptypes.IsolatedMargin {
		return perptypes.HealthHealthy, nil
	}
	rp, err := k.ComputeIsolatedRisk(ctx, accountIdx, marketIdx)
	if err != nil {
		return 0, err
	}
	return classifyHealth(rp), nil
}

// IterateIsolatedPositions walks every isolated perp position held by
// the account and invokes `fn(marketIdx, status, rp)`. `fn` may return
// `true` to stop iteration. Used by liquidation/matching to flag /
// liquidate isolated positions independently of the cross health.
func (k Keeper) IterateIsolatedPositions(ctx context.Context, accountIdx uint64,
	fn func(marketIdx uint32, status uint32, rp types.RiskParameters) bool,
) error {
	for marketIdx := uint32(0); marketIdx <= perptypes.MaxPerpsMarketIndex; marketIdx++ {
		pos, err := k.accountKeeper.GetPosition(ctx, accountIdx, marketIdx)
		if err != nil {
			return err
		}
		if pos.Position.IsZero() || pos.MarginMode != perptypes.IsolatedMargin {
			continue
		}
		rp, err := k.ComputeIsolatedRisk(ctx, accountIdx, marketIdx)
		if err != nil {
			return err
		}
		if fn(marketIdx, classifyHealth(rp), rp) {
			return nil
		}
	}
	return nil
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

// GetTotalAccountValue returns TAV = collateral + sum(uPnL across CROSS
// markets) for the account. Used by public-pool share-value math
// (NAV = TAV / total_shares). Isolated positions are deliberately
// excluded, mirroring the spec's "isolated is a sub-account" rule.
func (k Keeper) GetTotalAccountValue(ctx context.Context, accountIdx uint64) (math.Int, error) {
	ri, err := k.ComputeRiskInfo(ctx, accountIdx)
	if err != nil {
		return math.ZeroInt(), err
	}
	cur := ri.CurrentRiskParameters
	if cur == nil {
		return math.ZeroInt(), nil
	}
	return cur.TotalAccountValue, nil
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

// IsValidRiskChange enforces the post-state vs pre-state risk
// invariants. It walks both the cross account and each isolated
// position the account holds; if either side regresses the change is
// rejected.
//
// Per-side semantics (Lighter parity):
//
//   - HEALTHY post-state is accepted unconditionally.
//   - PRE_LIQUIDATION pre-state: post must remain at most PRE,
//     post.MMR <= pre.MMR (no new exposure on the same mark), AND
//     TAV/MMR ratio cannot decrease. This implements the spec's
//     "do not increase the size of any position and do not decrease
//     the account value to maintenance margin requirement ratio"
//     rule. Mark prices are constant across pre/post inside the same
//     block, so the MMR comparison is equivalent to a per-position
//     |size| comparison.
//   - Otherwise (PARTIAL/FULL/BANKRUPTCY pre-state): post.class <=
//     pre.class AND TAV/IM ratio cannot decrease. Routine user trades
//     in these states are rejected up-front by the matching layer; the
//     check here is the safety net for liquidation-initiated fills.
func (k Keeper) IsValidRiskChange(ctx context.Context, accountIdx uint64) (bool, error) {
	if ok, err := k.isCrossRiskChangeValid(ctx, accountIdx); err != nil || !ok {
		return ok, err
	}
	// Walk each isolated position and require it to satisfy the same
	// invariants. We do not error when a pre-snapshot is missing for
	// an isolated position: the position may have just been opened
	// in this fill, so we fall back to "post must be HEALTHY".
	for marketIdx := uint32(0); marketIdx <= perptypes.MaxPerpsMarketIndex; marketIdx++ {
		pos, err := k.accountKeeper.GetPosition(ctx, accountIdx, marketIdx)
		if err != nil {
			return false, err
		}
		if pos.Position.IsZero() || pos.MarginMode != perptypes.IsolatedMargin {
			continue
		}
		ok, err := k.isIsolatedRiskChangeValid(ctx, accountIdx, marketIdx)
		if err != nil || !ok {
			return ok, err
		}
	}
	return true, nil
}

func (k Keeper) isCrossRiskChangeValid(ctx context.Context, accountIdx uint64) (bool, error) {
	post, err := k.ComputeRiskInfo(ctx, accountIdx)
	if err != nil {
		return false, err
	}
	postP := post.CurrentRiskParameters
	pre, err := k.Cache.Get(ctx, accountIdx)
	if err != nil {
		if errors.Is(err, collections.ErrNotFound) {
			return classifyChange(types.RiskParameters{}, *postP, true /*missingPre*/), nil
		}
		return false, err
	}
	return classifyChange(pre, *postP, false), nil
}

func (k Keeper) isIsolatedRiskChangeValid(ctx context.Context, accountIdx uint64, marketIdx uint32) (bool, error) {
	postRP, err := k.ComputeIsolatedRisk(ctx, accountIdx, marketIdx)
	if err != nil {
		return false, err
	}
	pre, err := k.IsolatedCache.Get(ctx, collections.Join(accountIdx, marketIdx))
	if err != nil {
		if errors.Is(err, collections.ErrNotFound) {
			return classifyChange(types.RiskParameters{}, postRP, true), nil
		}
		return false, err
	}
	return classifyChange(pre, postRP, false), nil
}

// classifyChange centralises the pre-vs-post risk decision used by both
// the cross and isolated paths. `missingPre` signals that no pre-state
// snapshot exists; in that case we reject any unhealthy post-state to
// avoid silently accepting a change that may have introduced the
// underwater state.
func classifyChange(pre, post types.RiskParameters, missingPre bool) bool {
	postClass := classifyHealth(post)
	if postClass == perptypes.HealthHealthy {
		return true
	}
	if missingPre {
		return false
	}
	preClass := classifyHealth(pre)
	if postClass > preClass {
		return false
	}
	switch preClass {
	case perptypes.HealthPreLiquidation:
		// Lighter PRE rule: no MMR growth + TAV/MMR ratio non-
		// decreasing. The MMR cap implicitly forbids any |size|
		// increase since mark is constant within the block.
		if post.MaintenanceMarginRequirement.GT(pre.MaintenanceMarginRequirement) {
			return false
		}
		if pre.MaintenanceMarginRequirement.IsZero() ||
			post.MaintenanceMarginRequirement.IsZero() {
			return true
		}
		lhs := post.TotalAccountValue.Mul(pre.MaintenanceMarginRequirement)
		rhs := pre.TotalAccountValue.Mul(post.MaintenanceMarginRequirement)
		return !lhs.LT(rhs)
	default:
		// PARTIAL / FULL / BANKRUPTCY pre-state: keep the historical
		// TAV/IM ratio safety net so liquidation fills can never
		// worsen efficiency.
		if post.InitialMarginRequirement.IsZero() ||
			pre.InitialMarginRequirement.IsZero() {
			return true
		}
		lhs := post.TotalAccountValue.Mul(pre.InitialMarginRequirement)
		rhs := pre.TotalAccountValue.Mul(post.InitialMarginRequirement)
		return !lhs.LT(rhs)
	}
}

// SnapshotPreRisk caches the pre-state RiskParameters for an account so
// IsValidRiskChange can compare after handlers run. Both the cross
// aggregate and every isolated position are snapshotted.
func (k Keeper) SnapshotPreRisk(ctx context.Context, accountIdx uint64) error {
	post, err := k.ComputeRiskInfo(ctx, accountIdx)
	if err != nil {
		return err
	}
	if post.CurrentRiskParameters != nil {
		if err := k.Cache.Set(ctx, accountIdx, *post.CurrentRiskParameters); err != nil {
			return err
		}
	}
	for marketIdx := uint32(0); marketIdx <= perptypes.MaxPerpsMarketIndex; marketIdx++ {
		pos, err := k.accountKeeper.GetPosition(ctx, accountIdx, marketIdx)
		if err != nil {
			return err
		}
		if pos.Position.IsZero() || pos.MarginMode != perptypes.IsolatedMargin {
			continue
		}
		rp, err := k.ComputeIsolatedRisk(ctx, accountIdx, marketIdx)
		if err != nil {
			return err
		}
		if err := k.IsolatedCache.Set(ctx, collections.Join(accountIdx, marketIdx), rp); err != nil {
			return err
		}
	}
	return nil
}

// GetPositionZeroPrice returns the price at which liquidating a portion
// of the position would leave the account's TAV/MMR ratio invariant —
// i.e. the "zero price" defined in the Lighter spec:
//
//	zeroPrice_long  = mark * (1 - sign(pos) * M_i * TAV / MMR)
//	zeroPrice_short = mark * (1 + |sign(pos)| * M_i * TAV / MMR)
//
// where:
//
//   - `mark` is the live mark price for the market;
//   - `M_i` is the maintenance margin fraction (basis points / MarginTick);
//   - `TAV` is the total account value of the relevant scope (cross
//     account aggregate for cross positions; AllocatedMargin + uPnL of
//     the isolated position for isolated positions);
//   - `MMR` is the corresponding maintenance margin requirement.
//
// The return is the unsigned uint32 price used by the orderbook engine.
// Bankrupt accounts (TAV < 0) are not partially liquidatable; callers
// must short-circuit before invoking this.
func (k Keeper) GetPositionZeroPrice(ctx context.Context, accountIdx uint64, marketIdx uint32) (uint32, error) {
	pos, err := k.accountKeeper.GetPosition(ctx, accountIdx, marketIdx)
	if err != nil {
		return 0, err
	}
	if pos.Position.IsZero() {
		return 0, nil
	}
	px, err := k.oracleKeeper.GetFreshPrice(ctx, marketIdx)
	if err != nil {
		return 0, types.ErrMissingPrice.Wrapf(
			"account=%d market=%d: %s", accountIdx, marketIdx, err.Error(),
		)
	}
	if px.MarkPrice == 0 {
		return 0, types.ErrZeroMarkPrice.Wrapf(
			"account=%d market=%d", accountIdx, marketIdx,
		)
	}
	md, err := k.marketKeeper.GetMarketDetails(ctx, marketIdx)
	if err != nil {
		return 0, err
	}

	var tav, mmr math.Int
	if pos.MarginMode == perptypes.IsolatedMargin {
		rp, err := k.ComputeIsolatedRisk(ctx, accountIdx, marketIdx)
		if err != nil {
			return 0, err
		}
		tav = rp.TotalAccountValue
		mmr = rp.MaintenanceMarginRequirement
	} else {
		ri, err := k.ComputeRiskInfo(ctx, accountIdx)
		if err != nil {
			return 0, err
		}
		if ri.CrossRiskParameters == nil {
			return uint32(px.MarkPrice), nil
		}
		tav = ri.CrossRiskParameters.TotalAccountValue
		mmr = ri.CrossRiskParameters.MaintenanceMarginRequirement
	}

	mark := math.NewIntFromUint64(uint64(px.MarkPrice))
	// Degenerate case: no maintenance requirement (only happens when
	// the position has been fully closed — not reachable here since
	// pos.Position.IsZero is guarded above — or for malformed market
	// configs). Fall back to the mark.
	if mmr.IsZero() {
		return uint32(px.MarkPrice), nil
	}
	// adjustment = mark * M_i * TAV / (MMR * MarginTick).
	// adjustment carries the SIGN of TAV; we then add or subtract it
	// based on the position direction.
	mi := math.NewIntFromUint64(uint64(md.MaintenanceMarginFraction))
	tickBig := math.NewIntFromUint64(uint64(perptypes.MarginTick))
	num := mark.Mul(mi).Mul(tav)
	denom := mmr.Mul(tickBig)
	adjustment := quoTowardZero(num, denom)

	var zp math.Int
	if pos.Position.IsNegative() {
		// Short: zeroPrice = mark * (1 + M·TAV/MMR).
		zp = mark.Add(adjustment)
	} else {
		// Long: zeroPrice = mark * (1 - M·TAV/MMR).
		zp = mark.Sub(adjustment)
	}
	if zp.IsNegative() || zp.IsZero() {
		return 1, nil
	}
	maxPrice := math.NewIntFromUint64(uint64(perptypes.MaxOrderPrice))
	if zp.GT(maxPrice) {
		return perptypes.MaxOrderPrice, nil
	}
	return uint32(zp.Uint64()), nil
}

// quoTowardZero divides `num/denom` rounding toward zero so that signed
// adjustments behave symmetrically (math.Int.Quo uses Go-style
// truncated division which already truncates toward zero, but we wrap
// it for clarity and to make the intent explicit when num is negative).
func quoTowardZero(num, denom math.Int) math.Int {
	if denom.IsZero() {
		return math.ZeroInt()
	}
	return num.Quo(denom)
}

// GetPositionMarkValue returns |position| * mark_price as a math.Int.
// Returns zero when no position exists; errors out on missing/stale oracle.
func (k Keeper) GetPositionMarkValue(ctx context.Context, accountIdx uint64, marketIdx uint32) (math.Int, error) {
	pos, err := k.accountKeeper.GetPosition(ctx, accountIdx, marketIdx)
	if err != nil {
		return math.ZeroInt(), err
	}
	if pos.Position.IsZero() {
		return math.ZeroInt(), nil
	}
	px, err := k.oracleKeeper.GetFreshPrice(ctx, marketIdx)
	if err != nil {
		return math.ZeroInt(), types.ErrMissingPrice.Wrapf(
			"account=%d market=%d: %s", accountIdx, marketIdx, err.Error(),
		)
	}
	if px.MarkPrice == 0 {
		return math.ZeroInt(), types.ErrZeroMarkPrice
	}
	return pos.Position.Abs().Mul(math.NewIntFromUint64(uint64(px.MarkPrice))), nil
}

// GetPositionUnrealizedPnL returns the signed unrealized PnL of the
// (account, market) position at the current mark price:
//
//	uPnL = position * mark_price - entry_quote
//
// Positive when the position is in profit. Returns zero when no position
// exists or no mark price is available.
func (k Keeper) GetPositionUnrealizedPnL(ctx context.Context, accountIdx uint64, marketIdx uint32) (math.Int, error) {
	pos, err := k.accountKeeper.GetPosition(ctx, accountIdx, marketIdx)
	if err != nil {
		return math.ZeroInt(), err
	}
	if pos.Position.IsZero() {
		return math.ZeroInt(), nil
	}
	px, err := k.oracleKeeper.GetFreshPrice(ctx, marketIdx)
	if err != nil {
		return math.ZeroInt(), types.ErrMissingPrice.Wrapf(
			"account=%d market=%d: %s", accountIdx, marketIdx, err.Error(),
		)
	}
	if px.MarkPrice == 0 {
		return math.ZeroInt(), types.ErrZeroMarkPrice
	}
	return pos.Position.Mul(math.NewIntFromUint64(uint64(px.MarkPrice))).Sub(pos.EntryQuote), nil
}

// SimulateRiskAfterTakeover computes what the account's CROSS risk
// parameters would look like if `delta` (signed base size) of `marketIdx`
// were ADDED to the account's existing position at `entryPrice`. This
// is used by the LLP/insurance-fund take-over routine to preview
// whether absorbing a victim's position would push the LLP below its
// initial margin requirement.
//
// `entryPrice` is the price at which the takeover would be settled
// (typically the victim's zero price). `delta` carries the sign of the
// position the LLP would inherit.
//
// The simulation ONLY updates the targeted position's |size| and
// entry_quote contribution to IM/MM/CM/uPnL; it does NOT mutate any
// state. Returned RiskParameters are the would-be cross aggregates.
func (k Keeper) SimulateRiskAfterTakeover(
	ctx context.Context,
	accountIdx uint64,
	marketIdx uint32,
	delta math.Int,
	entryPrice uint32,
) (types.RiskParameters, error) {
	base, err := k.ComputeRiskInfo(ctx, accountIdx)
	if err != nil {
		return types.RiskParameters{}, err
	}
	cur := types.RiskParameters{}
	if base.CurrentRiskParameters != nil {
		cur = *base.CurrentRiskParameters
	}
	if delta.IsZero() {
		return cur, nil
	}
	pos, err := k.accountKeeper.GetPosition(ctx, accountIdx, marketIdx)
	if err != nil {
		return types.RiskParameters{}, err
	}
	if pos.MarginMode == perptypes.IsolatedMargin {
		// LLP / IF positions are always cross-margined; refusing here
		// surfaces the misconfiguration.
		return types.RiskParameters{}, fmt.Errorf("simulate_takeover: account %d market %d is isolated", accountIdx, marketIdx)
	}
	px, err := k.oracleKeeper.GetFreshPrice(ctx, marketIdx)
	if err != nil {
		return types.RiskParameters{}, types.ErrMissingPrice.Wrapf(
			"account=%d market=%d: %s", accountIdx, marketIdx, err.Error(),
		)
	}
	if px.MarkPrice == 0 {
		return types.RiskParameters{}, types.ErrZeroMarkPrice.Wrapf(
			"account=%d market=%d", accountIdx, marketIdx,
		)
	}
	md, err := k.marketKeeper.GetMarketDetails(ctx, marketIdx)
	if err != nil {
		return types.RiskParameters{}, err
	}
	markInt := math.NewIntFromUint64(uint64(px.MarkPrice))
	tickBig := math.NewInt(int64(perptypes.MarginTick))

	// Subtract the OLD contribution of (account, market) from cur.
	if !pos.Position.IsZero() {
		oldNotional := pos.Position.Abs().Mul(markInt)
		oldIM := oldNotional.Mul(math.NewIntFromUint64(uint64(md.DefaultInitialMarginFraction))).Quo(tickBig)
		oldMM := oldNotional.Mul(math.NewIntFromUint64(uint64(md.MaintenanceMarginFraction))).Quo(tickBig)
		oldCM := oldNotional.Mul(math.NewIntFromUint64(uint64(md.CloseOutMarginFraction))).Quo(tickBig)
		oldUPnL := pos.Position.Mul(markInt).Sub(pos.EntryQuote)
		cur.InitialMarginRequirement = cur.InitialMarginRequirement.Sub(oldIM)
		cur.MaintenanceMarginRequirement = cur.MaintenanceMarginRequirement.Sub(oldMM)
		cur.CloseOutMarginRequirement = cur.CloseOutMarginRequirement.Sub(oldCM)
		cur.TotalAccountValue = cur.TotalAccountValue.Sub(oldUPnL)
	}
	// Apply the simulated delta + entry to derive a NEW position.
	entryInt := math.NewIntFromUint64(uint64(entryPrice))
	newSize := pos.Position.Add(delta)
	// Compute entry_quote of the resulting position. This mirrors
	// applyPositionChange's logic for the increase / decrease / flip
	// scenarios but in pure math.
	var newEntryQuote math.Int
	switch {
	case pos.Position.IsZero():
		newEntryQuote = delta.Mul(entryInt)
	case sameSignInt(pos.Position, delta):
		newEntryQuote = pos.EntryQuote.Add(delta.Mul(entryInt))
	case newSize.IsZero() || sameSignInt(pos.Position, newSize):
		newEntryQuote = pos.EntryQuote.Mul(newSize).Quo(pos.Position)
	default:
		newEntryQuote = newSize.Mul(entryInt)
	}
	if !newSize.IsZero() {
		newNotional := newSize.Abs().Mul(markInt)
		newIM := newNotional.Mul(math.NewIntFromUint64(uint64(md.DefaultInitialMarginFraction))).Quo(tickBig)
		newMM := newNotional.Mul(math.NewIntFromUint64(uint64(md.MaintenanceMarginFraction))).Quo(tickBig)
		newCM := newNotional.Mul(math.NewIntFromUint64(uint64(md.CloseOutMarginFraction))).Quo(tickBig)
		newUPnL := newSize.Mul(markInt).Sub(newEntryQuote)
		cur.InitialMarginRequirement = cur.InitialMarginRequirement.Add(newIM)
		cur.MaintenanceMarginRequirement = cur.MaintenanceMarginRequirement.Add(newMM)
		cur.CloseOutMarginRequirement = cur.CloseOutMarginRequirement.Add(newCM)
		cur.TotalAccountValue = cur.TotalAccountValue.Add(newUPnL)
	}
	return cur, nil
}

func sameSignInt(a, b math.Int) bool {
	if a.IsZero() || b.IsZero() {
		return false
	}
	return a.IsNegative() == b.IsNegative()
}
