package keeper

import (
	"context"
	"errors"
	"fmt"

	"cosmossdk.io/collections"
	"cosmossdk.io/core/store"
	sdkerrors "cosmossdk.io/errors"
	"cosmossdk.io/math"

	"github.com/cosmos/cosmos-sdk/codec"

	perptypes "github.com/perpdex/perpdex-l1/types"
	accounttypes "github.com/perpdex/perpdex-l1/x/account/types"
	"github.com/perpdex/perpdex-l1/x/trade/types"
)

// Keeper provides pure trade application functions used by x/matching and
// x/liquidation. It owns no state apart from Params.
type Keeper struct {
	cdc           codec.BinaryCodec
	storeService  store.KVStoreService
	authority     string
	accountKeeper types.AccountKeeper
	marketKeeper  types.MarketKeeper
	fundingKeeper types.FundingKeeper
	riskKeeper    types.RiskKeeper

	Schema collections.Schema
	Params collections.Item[types.Params]
}

func NewKeeper(cdc codec.BinaryCodec, storeService store.KVStoreService, authority string,
	ak types.AccountKeeper, mk types.MarketKeeper, fk types.FundingKeeper, rk types.RiskKeeper,
) Keeper {
	sb := collections.NewSchemaBuilder(storeService)
	k := Keeper{
		cdc:           cdc,
		storeService:  storeService,
		authority:     authority,
		accountKeeper: ak,
		marketKeeper:  mk,
		fundingKeeper: fk,
		riskKeeper:    rk,

		Params: collections.NewItem(sb, types.ParamsKey, "params", codec.CollValue[types.Params](cdc)),
	}
	schema, err := sb.Build()
	if err != nil {
		panic(fmt.Errorf("trade: %w", err))
	}
	k.Schema = schema
	return k
}

func (k Keeper) Authority() string { return k.authority }

// Fill is the input to ApplyPerpsMatching / ApplySpotMatching. It captures one
// match between a maker and a taker.
type Fill struct {
	MakerAccountIndex uint64
	TakerAccountIndex uint64
	MarketIndex       uint32
	Price             uint32
	BaseAmount        uint64
	IsTakerAsk        bool
	TakerFee          uint32
	MakerFee          uint32
	NoFee             bool // liquidation/deleverage path
	// NoRiskCheck skips the post-trade IsValidRiskChange call on the
	// taker and maker. Reserved for forced close-outs (market-expiry
	// exit, etc.) where the insurance fund or ADL counterparty must
	// absorb residual size even when doing so worsens its own health.
	NoRiskCheck bool
	// SkipMakerRiskCheck only skips the post-trade risk check on the
	// MAKER side. Used by the partial-liquidation path: the maker is
	// the victim — the fill strictly closes part of an unhealthy
	// position so it is guaranteed to improve health, but the
	// IsValidRiskChange routine would still reject because post is
	// not HEALTHY. The taker (liquidator) keeps its standard check.
	SkipMakerRiskCheck bool
	// ZeroPrice + LiquidationFeeBps + LiquidationFeeRecipient
	// describe the Lighter "improvement-over-zero-price" liquidation
	// fee. When LiquidationFeeBps > 0:
	//   improvement = sign * (Price - ZeroPrice) * BaseAmount   (taker
	//                                                            sign;
	//                                                            positive
	//                                                            when the
	//                                                            fill is
	//                                                            better
	//                                                            than the
	//                                                            zero price
	//                                                            for the
	//                                                            victim/maker)
	//   raw_fee     = improvement * LiquidationFeeBps / FeeTick
	//   fee         = min(raw_fee, BaseAmount * Price / 100)        (1% cap)
	//
	// `fee` is debited from the victim (maker) collateral and credited
	// to LiquidationFeeRecipient (the LLP / Insurance Fund). Standard
	// MakerFee/TakerFee are NOT applied on the same fill (caller sets
	// them to 0). Fee remains zero whenever Price == ZeroPrice — the
	// expected case for keeper-driven IoC closes that fill exactly at
	// the zero price.
	ZeroPrice               uint32
	LiquidationFeeBps       uint32
	LiquidationFeeRecipient uint64
}

// ApplyPerpsMatching applies a perp fill to both maker and taker positions.
// Implements the 8-step pipeline from 14-trade.md §3 with full lighter
// `is_valid_perps_trade` parity:
//  1. settle pending funding for both sides
//  2. snapshot pre-state risk
//  3. compute position deltas (4 scenarios) + bounds-check
//     `|position|` and `|entry_quote|` against POSITION_SIZE_BITS /
//     ENTRY_QUOTE_BITS (lighter `is_new_position_valid`)
//  4. route realized PnL: isolated → allocated_margin, cross → collateral
//  5. apply taker/maker fees + treasury (and liquidation improvement
//     fee when present)
//  6. for isolated positions, auto-allocate `margin_delta` from cross
//     collateral (lighter `calculate_isolated_margin_change`) and pre-
//     check `available_cross_collateral >= margin_delta`
//  7. update OI using `|position|` deltas (both sides, divided by 2)
//  8. validate IsValidRiskChange for BOTH taker and maker
//
// Each per-side failure is wrapped into the corresponding maker / taker
// sentinel so the matching loop can evict the bad maker (and continue)
// or stop the bad taker (preserving prior fills) per Lighter
// `cancel_maker_order` / `cancel_taker_order` semantics.
func (k Keeper) ApplyPerpsMatching(ctx context.Context, f Fill) error {
	if err := k.fundingKeeper.SettlePositionFunding(ctx, f.MakerAccountIndex, f.MarketIndex); err != nil {
		return err
	}
	if err := k.fundingKeeper.SettlePositionFunding(ctx, f.TakerAccountIndex, f.MarketIndex); err != nil {
		return err
	}
	if !f.NoRiskCheck {
		if err := k.riskKeeper.SnapshotPreRisk(ctx, f.MakerAccountIndex); err != nil {
			return err
		}
		if err := k.riskKeeper.SnapshotPreRisk(ctx, f.TakerAccountIndex); err != nil {
			return err
		}
	}

	makerSign := int64(1)
	if !f.IsTakerAsk {
		makerSign = -1
	}
	takerSign := -makerSign

	makerRes, err := k.applyPositionChange(ctx, f.MakerAccountIndex, f.MarketIndex, f.Price, f.BaseAmount, makerSign)
	if err != nil {
		if errors.Is(err, errPositionOutOfBounds) {
			return sdkerrors.Wrapf(types.ErrMakerInvalidPosition,
				"account %d market %d", f.MakerAccountIndex, f.MarketIndex)
		}
		return err
	}
	takerRes, err := k.applyPositionChange(ctx, f.TakerAccountIndex, f.MarketIndex, f.Price, f.BaseAmount, takerSign)
	if err != nil {
		if errors.Is(err, errPositionOutOfBounds) {
			return sdkerrors.Wrapf(types.ErrTakerInvalidPosition,
				"account %d market %d", f.TakerAccountIndex, f.MarketIndex)
		}
		return err
	}

	// Compute fees once so we can both route the per-side debit and
	// later feed the SAME fee value into the isolated-margin delta
	// calculation (lighter parity: `trade_pnl - fee` enters
	// `result_if_position_open_and_open_interest_increased`).
	notional := math.NewIntFromUint64(f.BaseAmount).Mul(math.NewIntFromUint64(uint64(f.Price)))
	var takerFee, makerFee math.Int
	if f.NoFee {
		takerFee = math.ZeroInt()
		makerFee = math.ZeroInt()
	} else {
		takerFee = notional.Mul(math.NewIntFromUint64(uint64(f.TakerFee))).Quo(math.NewInt(int64(perptypes.FeeTick)))
		makerFee = notional.Mul(math.NewIntFromUint64(uint64(f.MakerFee))).Quo(math.NewInt(int64(perptypes.FeeTick)))
	}

	// Route realized PnL + fee to the right pool (allocated_margin
	// for isolated positions, cross collateral for cross). This
	// mirrors lighter's `taker_collateral_delta` flowing into
	// `allocated_margin` for isolated and into cross collateral for
	// cross before the margin_delta auto-allocation step.
	if err := k.applyPositionFinancials(ctx, &takerRes, takerFee); err != nil {
		return err
	}
	if err := k.applyPositionFinancials(ctx, &makerRes, makerFee); err != nil {
		return err
	}

	// Treasury fee credit (sum of both sides). Treasury is the
	// `TreasuryAccountIndex` cross account regardless of whether
	// either side is isolated — the fee flows to a global pool.
	if !f.NoFee {
		treasuryFee := takerFee.Add(makerFee)
		if !treasuryFee.IsZero() {
			if err := k.accountKeeper.AddCollateral(ctx, perptypes.TreasuryAccountIndex, treasuryFee); err != nil {
				return err
			}
		}
		if f.LiquidationFeeBps > 0 {
			liqFee := liquidationImprovementFee(f, notional)
			if liqFee.IsPositive() {
				// The improvement fee is debited from the
				// victim (maker). For an isolated victim
				// position, take it out of the position's
				// allocated_margin so the cross account is not
				// disturbed; for cross take it out of cross
				// collateral. Either way, credit the LLP /
				// insurance fund recipient (always cross).
				if err := k.debitFromMarginPool(ctx, &makerRes, liqFee); err != nil {
					return err
				}
				recipient := f.LiquidationFeeRecipient
				if recipient == 0 {
					recipient = perptypes.InsuranceFundOperatorAccountIdx
				}
				if err := k.accountKeeper.AddCollateral(ctx, recipient, liqFee); err != nil {
					return err
				}
			}
		}
	}

	// Auto-allocate isolated margin (lighter
	// `calculate_isolated_margin_change`). For isolated positions,
	// compute `margin_delta` from old/new sizes + realized trade
	// PnL/fee, then move that much from cross collateral into
	// `allocated_margin`. Refuse the fill when the delta is positive
	// and available cross collateral is short (lighter parity:
	// `is_*_has_enough_cross_collateral`). The auto-allocation
	// honours the same skip-flags as the post-state risk check below
	// so the partial-liquidation path can still close out an isolated
	// underwater victim without the cross-collateral safety check.
	if err := k.applyIsolatedMargin(ctx, &takerRes, takerFee, false /*isMaker*/, f); err != nil {
		return err
	}
	if err := k.applyIsolatedMargin(ctx, &makerRes, makerFee, true /*isMaker*/, f); err != nil {
		return err
	}

	// Open interest = sum over accounts of |position|, divided by 2 since
	// every fill touches exactly two accounts. Using the |newSize|-|oldSize|
	// delta ensures round-trips (open then close) return OI to its original
	// value rather than linearly growing with cumulative fill volume.
	oiDelta := (makerRes.OIDelta + takerRes.OIDelta) / 2
	if err := k.marketKeeper.UpdateOpenInterest(ctx, f.MarketIndex, oiDelta); err != nil {
		return err
	}

	if f.NoRiskCheck {
		return nil
	}
	// Both maker and taker must pass the post-state risk check: makers
	// resting on the book otherwise have an open attack vector where a
	// low-collateral maker lets the book close against them into a fresh
	// unhealthy position. Lighter parity: l2_trade enforces both sides.
	//
	// Exception: when the fill is the partial-liquidation closing leg
	// (SkipMakerRiskCheck), the maker IS the victim — the trade
	// mechanically improves their TAV/MMR ratio (the fill price is at
	// or better than the zero price, by construction). The
	// IsValidRiskChange routine still rejects an unhealthy post-state,
	// so we skip it on the maker side and let the liquidation engine
	// be the authority on the close-out.
	for _, idx := range []uint64{f.TakerAccountIndex, f.MakerAccountIndex} {
		if f.SkipMakerRiskCheck && idx == f.MakerAccountIndex {
			continue
		}
		ok, err := k.riskKeeper.IsValidRiskChange(ctx, idx)
		if err != nil {
			return err
		}
		if !ok {
			// Classify the regression by side so the matching
			// loop can evict a bad maker (and continue) without
			// reverting the entire taker tx, while a bad taker
			// stops further fills but keeps the prior ones.
			if idx == f.MakerAccountIndex {
				return sdkerrors.Wrapf(types.ErrMakerRiskRegression,
					"account %d", idx)
			}
			return sdkerrors.Wrapf(types.ErrTakerRiskRegression,
				"account %d", idx)
		}
	}
	return nil
}

// liquidationImprovementFee computes the Lighter liquidation fee:
//
//	improvement_per_unit = sign(takerSide) * (price - zeroPrice)
//	improvement          = improvement_per_unit * BaseAmount
//	raw_fee              = improvement * LiquidationFeeBps / FeeTick
//	fee                  = clamp(raw_fee, 0, notional / 100)
//
// `takerSide` flips the improvement sign so that a fill BETTER than the
// victim's zero price yields a positive fee regardless of whether the
// taker is selling (closing the victim's long) or buying (closing the
// victim's short). When Price == ZeroPrice the improvement is zero and
// no fee is charged — matching the keeper-driven IoC close-out path
// where the engine fills exactly at the zero price.
func liquidationImprovementFee(f Fill, notional math.Int) math.Int {
	if f.LiquidationFeeBps == 0 || f.BaseAmount == 0 {
		return math.ZeroInt()
	}
	priceInt := math.NewIntFromUint64(uint64(f.Price))
	zpInt := math.NewIntFromUint64(uint64(f.ZeroPrice))
	var improvementPerUnit math.Int
	if f.IsTakerAsk {
		// Taker sells (maker/victim is being long-liquidated): a
		// HIGHER fill price than zero price is "better" for victim.
		improvementPerUnit = priceInt.Sub(zpInt)
	} else {
		// Taker buys (maker/victim is being short-liquidated): a
		// LOWER fill price than zero price is "better" for victim.
		improvementPerUnit = zpInt.Sub(priceInt)
	}
	if !improvementPerUnit.IsPositive() {
		return math.ZeroInt()
	}
	improvement := improvementPerUnit.Mul(math.NewIntFromUint64(f.BaseAmount))
	rawFee := improvement.Mul(math.NewIntFromUint64(uint64(f.LiquidationFeeBps))).Quo(math.NewInt(int64(perptypes.FeeTick)))
	cap1pct := notional.Quo(math.NewInt(100))
	if rawFee.GT(cap1pct) {
		rawFee = cap1pct
	}
	return rawFee
}

// positionChangeResult bundles the inputs / outputs of one side's
// position update so the surrounding ApplyPerpsMatching pipeline can
// chain through the lighter
// `realized_pnl → fee → margin_delta → risk` sequence on the right
// account / market without re-loading state.
//
// `New` reflects the position AFTER size + entry_quote are written,
// but BEFORE the realized-PnL / fee / margin_delta routing — the
// helpers below mutate `New.AllocatedMargin` as they fold those flows
// in (and re-persist via `SetPosition` whenever necessary).
type positionChangeResult struct {
	AccountIdx  uint64
	MarketIdx   uint32
	Old         accounttypes.AccountPosition
	New         accounttypes.AccountPosition
	OIDelta     int64
	SideFlipped bool
	RealizedPnL math.Int
}

// errPositionOutOfBounds is the internal sentinel returned by
// `applyPositionChange` when the post-trade `|position|` or
// `|entry_quote|` would overflow `POSITION_SIZE_BITS` /
// `ENTRY_QUOTE_BITS` (lighter `is_new_position_valid` failure mode).
// `ApplyPerpsMatching` re-wraps it into `ErrMakerInvalidPosition` /
// `ErrTakerInvalidPosition` so the matching loop can route the failure
// through `IsRecoverable*Error`.
var errPositionOutOfBounds = errors.New("trade: post-trade position out of bounds")

// applyPositionChange handles the four position-change scenarios from
// 14-trade.md §3.2: open new, increase, decrease, flip. It computes the
// new position size + entry_quote and the realized PnL but does NOT
// route the realized PnL anywhere — `applyPositionFinancials` does
// that based on the position's margin mode (lighter parity).
//
// The returned `positionChangeResult` carries enough context for the
// caller to drive the rest of the lighter `apply_perps_trade` pipeline
// (fee routing, isolated margin auto-allocation, risk check).
//
// `errPositionOutOfBounds` is returned when the new size or entry
// quote would overflow the bit-width bounds enforced by the prover
// circuit; the caller wraps it into the appropriate maker / taker
// sentinel.
func (k Keeper) applyPositionChange(ctx context.Context, accountIdx uint64, marketIdx uint32, price uint32, baseAmount uint64, sign int64) (positionChangeResult, error) {
	pos, err := k.accountKeeper.GetPosition(ctx, accountIdx, marketIdx)
	if err != nil {
		return positionChangeResult{}, err
	}
	old := clonePosition(pos)
	curSize := pos.Position
	delta := math.NewIntFromUint64(baseAmount).MulRaw(sign)
	newSize := curSize.Add(delta)

	curEntryQuote := pos.EntryQuote
	if curEntryQuote.IsNil() {
		curEntryQuote = math.ZeroInt()
	}
	notional := math.NewIntFromUint64(baseAmount).Mul(math.NewIntFromUint64(uint64(price))).MulRaw(sign)

	realizedPnL := math.ZeroInt()
	switch {
	case curSize.IsZero():
		// open new position
		pos.EntryQuote = notional
	case sameSign(curSize, delta):
		// increase
		pos.EntryQuote = curEntryQuote.Add(notional)
	case newSize.IsZero() || sameSign(curSize, newSize):
		// pure decrease (or close): realize partial PnL
		realizedPnL = notional.Add(curEntryQuote.Mul(delta).Quo(curSize.Neg()))
		// scale entry_quote proportionally to remaining size
		if curSize.IsZero() {
			pos.EntryQuote = math.ZeroInt()
		} else {
			pos.EntryQuote = curEntryQuote.Mul(newSize).Quo(curSize)
		}
	default:
		// flip: close existing then open in opposite direction.
		//
		// Of the `baseAmount` units traded, `|curSize|` units close
		// the existing position and the remainder opens the new one
		// on the opposite side. The trade-side notional for the
		// closing portion is `closeBase * price * sign` (signed by
		// the trade direction, NOT the position direction): if the
		// trade is a sell, the closing leg also sells, so the
		// notional carries `sign = -1`. Using `-sign` here would
		// produce a +/- mismatch between `closeNotional` and
		// `curEntryQuote` and inflate `realized_pnl` by 2× the
		// closing leg's notional — corrupting PnL realization on
		// every flip.
		closeBase := curSize.Abs()
		closeNotional := closeBase.Mul(math.NewIntFromUint64(uint64(price))).MulRaw(sign)
		realizedPnL = closeNotional.Add(curEntryQuote)
		residual := delta.Add(curSize) // residual same sign as delta
		residualNotional := residual.Mul(math.NewIntFromUint64(uint64(price)))
		pos.EntryQuote = residualNotional
	}
	pos.Position = newSize

	// Bounds check ahead of `SetPosition` so we never persist a
	// position the prover circuit would reject.
	if !isWithinPositionBounds(pos.Position, pos.EntryQuote) {
		return positionChangeResult{}, errPositionOutOfBounds
	}

	if err := k.accountKeeper.SetPosition(ctx, pos); err != nil {
		return positionChangeResult{}, err
	}
	// OI contribution from this account: |new| - |old|. Positive when the
	// account grows its exposure, negative when reducing / closing.
	oiDelta := newSize.Abs().Sub(curSize.Abs())
	return positionChangeResult{
		AccountIdx:  accountIdx,
		MarketIdx:   marketIdx,
		Old:         old,
		New:         clonePosition(pos),
		OIDelta:     oiDelta.Int64(),
		SideFlipped: !curSize.IsZero() && !newSize.IsZero() && !sameSign(curSize, newSize),
		RealizedPnL: realizedPnL,
	}, nil
}

// applyPositionFinancials routes (`realized_pnl - fee`) to the right
// pool: into `allocated_margin` for an isolated position (lighter:
// `is_*_position_isolated` branch), or into cross collateral
// otherwise. Updates `res.New` in-place and re-persists the position
// when the allocated_margin changed so downstream
// `calculateIsolatedMarginDelta` can read the current state.
//
// `fee` is the per-side debit owed to the treasury (already non-
// negative in the caller). When the position closes (`new size == 0`
// for an isolated position) the realized PnL still flows through
// allocated_margin first; the subsequent `applyIsolatedMargin` step
// then releases everything back to cross via a negative margin_delta.
func (k Keeper) applyPositionFinancials(ctx context.Context, res *positionChangeResult, fee math.Int) error {
	delta := res.RealizedPnL
	if !fee.IsZero() {
		delta = delta.Sub(fee)
	}
	if delta.IsZero() {
		return nil
	}
	// Route based on the OLD position's margin mode. The lighter
	// circuit uses the `old_position.margin_mode` precisely because
	// it represents what bucket the account thought it was operating
	// in coming into the trade; flipping the routing on a position
	// that just opened mid-fill would be inconsistent.
	if res.Old.MarginMode == perptypes.IsolatedMargin {
		if res.New.AllocatedMargin.IsNil() {
			res.New.AllocatedMargin = math.ZeroInt()
		}
		res.New.AllocatedMargin = res.New.AllocatedMargin.Add(delta)
		return k.accountKeeper.SetPosition(ctx, res.New)
	}
	return k.accountKeeper.AddCollateral(ctx, res.AccountIdx, delta)
}

// debitFromMarginPool subtracts `amount` from the side's effective
// margin pool: `allocated_margin` for an isolated position, cross
// collateral otherwise. Used by the liquidation improvement-fee path
// so an isolated victim's cross account is not arbitrarily disturbed.
func (k Keeper) debitFromMarginPool(ctx context.Context, res *positionChangeResult, amount math.Int) error {
	if amount.IsZero() {
		return nil
	}
	if res.Old.MarginMode == perptypes.IsolatedMargin {
		if res.New.AllocatedMargin.IsNil() {
			res.New.AllocatedMargin = math.ZeroInt()
		}
		res.New.AllocatedMargin = res.New.AllocatedMargin.Sub(amount)
		return k.accountKeeper.SetPosition(ctx, res.New)
	}
	return k.accountKeeper.AddCollateral(ctx, res.AccountIdx, amount.Neg())
}

// applyIsolatedMargin computes the lighter
// `calculate_isolated_margin_change` delta for an isolated position
// and applies it: `allocated_margin += margin_delta`,
// `cross_collateral -= margin_delta`. When the delta is positive (the
// position needs MORE margin), the available cross USDC collateral is
// pre-checked via the risk keeper; insufficient headroom surfaces as
// `ErrMakerInsufficientCollateral` / `ErrTakerInsufficientCollateral`
// for the matching loop to evict the maker / stop the taker.
//
// Cross-margined positions are no-ops here.
//
// `SkipMakerRiskCheck` (and `NoRiskCheck`) skip the cross-collateral
// availability check on the maker side so the partial-liquidation
// path can still close out an isolated underwater victim. The margin
// delta itself is still applied so allocated_margin / cross collateral
// reflect the close-out's accounting cleanly.
func (k Keeper) applyIsolatedMargin(ctx context.Context, res *positionChangeResult, fee math.Int, isMaker bool, f Fill) error {
	if res.Old.MarginMode != perptypes.IsolatedMargin {
		return nil
	}
	delta, err := k.calculateIsolatedMarginDelta(ctx, res, fee)
	if err != nil {
		return err
	}
	if delta.IsZero() {
		return nil
	}
	if delta.IsPositive() {
		skip := f.NoRiskCheck || (isMaker && f.SkipMakerRiskCheck)
		if !skip {
			avail, err := k.riskKeeper.GetAvailableUsdcCollateral(ctx, res.AccountIdx)
			if err != nil {
				return err
			}
			if avail.LT(delta) {
				if isMaker {
					return sdkerrors.Wrapf(types.ErrMakerInsufficientCollateral,
						"account %d available %s need %s",
						res.AccountIdx, avail.String(), delta.String())
				}
				return sdkerrors.Wrapf(types.ErrTakerInsufficientCollateral,
					"account %d available %s need %s",
					res.AccountIdx, avail.String(), delta.String())
			}
		}
	}
	if res.New.AllocatedMargin.IsNil() {
		res.New.AllocatedMargin = math.ZeroInt()
	}
	res.New.AllocatedMargin = res.New.AllocatedMargin.Add(delta)
	if err := k.accountKeeper.SetPosition(ctx, res.New); err != nil {
		return err
	}
	return k.accountKeeper.AddCollateral(ctx, res.AccountIdx, delta.Neg())
}

// calculateIsolatedMarginDelta is the in-Go equivalent of lighter
// `calculate_isolated_margin_change` for one side. Returns the signed
// math.Int amount that must be added to the position's
// `allocated_margin` (and removed from cross collateral) to keep the
// isolated position correctly margined after the fill:
//
//   - new position closed: -max(allocated_margin, 0)  (release the
//     remainder back to cross)
//   - side flipped: position_requirement - (allocated_margin +
//     uPnL_new)  (re-margin the new opposite-side position)
//   - same side, OI grew: max(0, oi_requirement - trade_pnl) where
//     trade_pnl = uPnL_new - uPnL_old - fee  (top up by the
//     incremental IM the fill consumed, less any PnL it generated)
//   - same side, OI shrank: -min( max(0, new_market_value -
//     target_value), max(allocated_margin, 0) ) where target_value =
//     max(ceil(old_market_value * |new| / |old|), position_requirement)
//     (release the proportional excess but never below the new
//     position's IM)
//
// `fee` is the per-side debit (in collateral units) the trade just
// paid. `res.New.AllocatedMargin` MUST already include the
// (realized_pnl - fee) credit produced by `applyPositionFinancials`,
// matching lighter's ordering where the
// `taker_collateral_delta`-adjusted allocated_margin feeds into
// `calculate_isolated_margin_change`.
func (k Keeper) calculateIsolatedMarginDelta(ctx context.Context, res *positionChangeResult, fee math.Int) (math.Int, error) {
	newPos := res.New
	oldPos := res.Old
	allocated := newPos.AllocatedMargin
	if allocated.IsNil() {
		allocated = math.ZeroInt()
	}

	// case 1: new position closed → release positive allocated_margin
	if newPos.Position.IsZero() {
		if allocated.IsPositive() {
			return allocated.Neg(), nil
		}
		return math.ZeroInt(), nil
	}

	posReq, err := k.riskKeeper.ComputePositionInitialMargin(ctx, res.MarketIdx, newPos.Position.Abs())
	if err != nil {
		return math.ZeroInt(), err
	}

	// case 2: side flipped → re-margin to position_requirement at the
	// new uPnL-adjusted account state.
	if res.SideFlipped {
		newUPnL, err := k.riskKeeper.ComputeUnrealizedPnLAt(ctx, res.MarketIdx, newPos.Position, newPos.EntryQuote)
		if err != nil {
			return math.ZeroInt(), err
		}
		return posReq.Sub(allocated.Add(newUPnL)), nil
	}

	if res.OIDelta < 0 {
		// case 4: same side, OI shrank → proportional release.
		oldUPnL, err := k.riskKeeper.ComputeUnrealizedPnLAt(ctx, res.MarketIdx, oldPos.Position, oldPos.EntryQuote)
		if err != nil {
			return math.ZeroInt(), err
		}
		newUPnL, err := k.riskKeeper.ComputeUnrealizedPnLAt(ctx, res.MarketIdx, newPos.Position, newPos.EntryQuote)
		if err != nil {
			return math.ZeroInt(), err
		}
		oldAllocated := oldPos.AllocatedMargin
		if oldAllocated.IsNil() {
			oldAllocated = math.ZeroInt()
		}
		oldMV := oldAllocated.Add(oldUPnL)
		newMV := allocated.Add(newUPnL)

		var targetValue math.Int
		oldAbs := oldPos.Position.Abs()
		newAbs := newPos.Position.Abs()
		if oldMV.IsPositive() && !oldAbs.IsZero() {
			// ceil_div(oldMV * |new|, |old|).
			num := oldMV.Mul(newAbs)
			targetValue = ceilDivPositive(num, oldAbs)
			if targetValue.LT(posReq) {
				targetValue = posReq
			}
		} else {
			// oldMV <= 0 ⇒ proportional value collapses to
			// position_requirement (lighter `MAX(target, posReq)`
			// with the negative-target shortcut).
			targetValue = posReq
		}

		excess := newMV.Sub(targetValue)
		if excess.IsNegative() {
			excess = math.ZeroInt()
		}
		toMoveOut := allocated
		if toMoveOut.IsNegative() {
			toMoveOut = math.ZeroInt()
		}
		if excess.GT(toMoveOut) {
			excess = toMoveOut
		}
		if excess.IsZero() {
			return math.ZeroInt(), nil
		}
		return excess.Neg(), nil
	}

	// case 3: same side, OI grew (or stayed flat). Top up by the
	// incremental IM less any PnL the fill itself generated.
	oiAbs := math.NewInt(res.OIDelta).Abs()
	if oiAbs.IsZero() {
		return math.ZeroInt(), nil
	}
	oiReq, err := k.riskKeeper.ComputePositionInitialMargin(ctx, res.MarketIdx, oiAbs)
	if err != nil {
		return math.ZeroInt(), err
	}
	oldUPnL, err := k.riskKeeper.ComputeUnrealizedPnLAt(ctx, res.MarketIdx, oldPos.Position, oldPos.EntryQuote)
	if err != nil {
		return math.ZeroInt(), err
	}
	newUPnL, err := k.riskKeeper.ComputeUnrealizedPnLAt(ctx, res.MarketIdx, newPos.Position, newPos.EntryQuote)
	if err != nil {
		return math.ZeroInt(), err
	}
	tradePnL := newUPnL.Sub(oldUPnL).Sub(fee)
	delta := oiReq.Sub(tradePnL)
	if delta.IsNegative() {
		return math.ZeroInt(), nil
	}
	return delta, nil
}

// isWithinPositionBounds enforces the prover circuit's hard limits
// `|position| < 2^POSITION_SIZE_BITS` and `|entry_quote| < 2^ENTRY_QUOTE_BITS`.
// Lighter `position.is_valid` checks the same envelope.
func isWithinPositionBounds(position, entryQuote math.Int) bool {
	if position.IsNil() {
		position = math.ZeroInt()
	}
	if entryQuote.IsNil() {
		entryQuote = math.ZeroInt()
	}
	maxPos := math.NewIntFromUint64(perptypes.MaxPositionSize)
	maxEntryQuote := math.NewIntFromUint64(perptypes.MaxEntryQuote)
	if position.Abs().GT(maxPos) {
		return false
	}
	if entryQuote.Abs().GT(maxEntryQuote) {
		return false
	}
	return true
}

// clonePosition returns a value copy with all math.Int fields
// guaranteed non-nil so downstream arithmetic doesn't blow up on a
// freshly-defaulted record.
func clonePosition(p accounttypes.AccountPosition) accounttypes.AccountPosition {
	out := p
	if out.Position.IsNil() {
		out.Position = math.ZeroInt()
	}
	if out.EntryQuote.IsNil() {
		out.EntryQuote = math.ZeroInt()
	}
	if out.LastFundingRatePrefixSum.IsNil() {
		out.LastFundingRatePrefixSum = math.ZeroInt()
	}
	if out.AllocatedMargin.IsNil() {
		out.AllocatedMargin = math.ZeroInt()
	}
	return out
}

// ceilDivPositive returns ⌈num/den⌉ for non-negative `num` and
// strictly positive `den`. Mirrors lighter `ceil_div_biguint` on the
// non-negative branch (the negative-numerator branch is handled in
// `calculateIsolatedMarginDelta` via the `oldMV <= 0` short-circuit).
func ceilDivPositive(num, den math.Int) math.Int {
	if den.IsZero() {
		return math.ZeroInt()
	}
	q := num.Quo(den)
	r := num.Mod(den)
	if r.IsZero() {
		return q
	}
	return q.Add(math.OneInt())
}

func sameSign(a, b math.Int) bool {
	if a.IsZero() || b.IsZero() {
		return false
	}
	return a.IsNegative() == b.IsNegative()
}

// ApplySpotMatching applies a spot fill: taker gives quote, gets base (buy)
// or vice versa (sell). UNIFIED collateral mode keeps account.collateral and
// account_asset.balance synchronized.
//
// The maker side debits its locked balance first (lock-on-place semantics
// from x/orderbook OpenOrder), spilling into available balance only if the
// caller forgot to lock — defensive parity with Lighter where resting
// orders always have their resources locked. The taker side debits its
// available balance directly.
//
// Insufficient-balance errors are wrapped into Maker* / Taker* sentinels
// so the matching loop can evict a bad maker and continue, or stop a bad
// taker without reverting prior fills.
func (k Keeper) ApplySpotMatching(ctx context.Context, f Fill, baseAssetID, quoteAssetID uint32) error {
	notional := math.NewIntFromUint64(f.BaseAmount).Mul(math.NewIntFromUint64(uint64(f.Price)))
	baseAmt := math.NewIntFromUint64(f.BaseAmount)
	if f.IsTakerAsk {
		// taker sells base, maker buys base — maker owes quote
		// (locked at place time), taker owes base (unlocked).
		if err := k.spotMakerDebit(ctx, f.MakerAccountIndex, f.TakerAccountIndex, quoteAssetID, notional); err != nil {
			return err
		}
		if err := k.spotTakerDebit(ctx, f.TakerAccountIndex, f.MakerAccountIndex, baseAssetID, baseAmt); err != nil {
			return err
		}
	} else {
		// taker buys base, maker sells base — maker owes base
		// (locked at place time), taker owes quote (unlocked).
		if err := k.spotMakerDebit(ctx, f.MakerAccountIndex, f.TakerAccountIndex, baseAssetID, baseAmt); err != nil {
			return err
		}
		if err := k.spotTakerDebit(ctx, f.TakerAccountIndex, f.MakerAccountIndex, quoteAssetID, notional); err != nil {
			return err
		}
	}

	if !f.NoFee {
		takerFee := notional.Mul(math.NewIntFromUint64(uint64(f.TakerFee))).Quo(math.NewInt(int64(perptypes.FeeTick)))
		makerFee := notional.Mul(math.NewIntFromUint64(uint64(f.MakerFee))).Quo(math.NewInt(int64(perptypes.FeeTick)))
		if takerFee.IsPositive() {
			if err := k.spotTakerDebit(ctx, f.TakerAccountIndex, perptypes.TreasuryAccountIndex, quoteAssetID, takerFee); err != nil {
				return err
			}
		}
		if makerFee.IsPositive() {
			// Maker fee is paid out of whatever quote balance the
			// maker still has after the lock release; debiting
			// from available is correct because the lock only
			// covered notional, not fees.
			if err := k.spotMakerDebit(ctx, f.MakerAccountIndex, perptypes.TreasuryAccountIndex, quoteAssetID, makerFee); err != nil {
				return err
			}
		}
	}
	return nil
}

// spotMakerDebit moves `amount` of `assetID` from `from` (a maker) to
// `to`, draining the maker's locked balance first (lock-on-place
// accounting from x/orderbook.OpenOrder) and falling back to the
// available balance only if the lock is short — defensive parity with
// Lighter where resting orders always have their resources locked.
//
// Insufficient-balance errors are wrapped into ErrMakerInsufficientBalance
// so the matching loop can evict the bad maker and continue.
func (k Keeper) spotMakerDebit(ctx context.Context, from, to uint64, assetID uint32, amount math.Int) error {
	if amount.IsNegative() {
		return fmt.Errorf("trade: transfer amount must be non-negative")
	}
	src, err := k.accountKeeper.GetAccountAsset(ctx, from, assetID)
	if err != nil {
		return err
	}
	if src.Balance.IsNil() {
		src.Balance = math.ZeroInt()
	}
	if src.LockedBalance.IsNil() {
		src.LockedBalance = math.ZeroInt()
	}
	if src.Balance.LT(amount) {
		return sdkerrors.Wrapf(types.ErrMakerInsufficientBalance,
			"account %d asset %d have %s need %s",
			from, assetID, src.Balance.String(), amount.String())
	}
	dst, err := k.accountKeeper.GetAccountAsset(ctx, to, assetID)
	if err != nil {
		return err
	}
	if dst.Balance.IsNil() {
		dst.Balance = math.ZeroInt()
	}
	// Drain the lock first so a partial fill releases the proportional
	// portion of resources reserved at place time.
	lockedDrain := amount
	if lockedDrain.GT(src.LockedBalance) {
		lockedDrain = src.LockedBalance
	}
	src.LockedBalance = src.LockedBalance.Sub(lockedDrain)
	src.Balance = src.Balance.Sub(amount)
	dst.Balance = dst.Balance.Add(amount)
	if err := k.accountKeeper.SetAccountAsset(ctx, src); err != nil {
		return err
	}
	return k.accountKeeper.SetAccountAsset(ctx, dst)
}

// spotTakerDebit moves `amount` of `assetID` from `from` (a taker) to
// `to`. Takers in spot matching are not lock-on-place (only resting
// orders lock), so the debit goes straight against the available
// balance.
//
// Insufficient-balance errors are wrapped into ErrTakerInsufficientBalance
// so the matching loop can stop the taker without reverting prior fills.
func (k Keeper) spotTakerDebit(ctx context.Context, from, to uint64, assetID uint32, amount math.Int) error {
	if amount.IsNegative() {
		return fmt.Errorf("trade: transfer amount must be non-negative")
	}
	src, err := k.accountKeeper.GetAccountAsset(ctx, from, assetID)
	if err != nil {
		return err
	}
	if src.Balance.IsNil() {
		src.Balance = math.ZeroInt()
	}
	available := src.Balance
	if !src.LockedBalance.IsNil() {
		available = available.Sub(src.LockedBalance)
	}
	if available.LT(amount) {
		return sdkerrors.Wrapf(types.ErrTakerInsufficientBalance,
			"account %d asset %d available %s need %s",
			from, assetID, available.String(), amount.String())
	}
	dst, err := k.accountKeeper.GetAccountAsset(ctx, to, assetID)
	if err != nil {
		return err
	}
	if dst.Balance.IsNil() {
		dst.Balance = math.ZeroInt()
	}
	src.Balance = src.Balance.Sub(amount)
	dst.Balance = dst.Balance.Add(amount)
	if err := k.accountKeeper.SetAccountAsset(ctx, src); err != nil {
		return err
	}
	return k.accountKeeper.SetAccountAsset(ctx, dst)
}

