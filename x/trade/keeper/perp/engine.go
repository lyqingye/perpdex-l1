package perp

import (
	"context"
	"errors"

	sdkerrors "cosmossdk.io/errors"
	"cosmossdk.io/math"

	perptypes "github.com/perpdex/perpdex-l1/types"
	"github.com/perpdex/perpdex-l1/x/trade/types"
)

// Engine encapsulates the perp trade-application pipeline. It owns no
// storage of its own; the surrounding x/trade keeper holds it as a
// composed value and forwards `ApplyPerpsMatching` calls into
// `Engine.Apply`.
//
// The fields mirror the keeper's expected-keepers wiring 1:1 so
// dependency surface stays uniform. Splitting the engine out lets us
// keep `package keeper` thin and route account-model-specific logic
// (cross / isolated / future unified) into per-mode files within
// this sub-package.
type Engine struct {
	accountKeeper types.AccountKeeper
	marketKeeper  types.MarketKeeper
	fundingKeeper types.FundingKeeper
	riskKeeper    types.RiskKeeper
}

// NewEngine wires the engine with the account / market / funding /
// risk keepers it needs. Pure constructor — no I/O, no schema work,
// the keeper-level builder owns those.
func NewEngine(
	ak types.AccountKeeper,
	mk types.MarketKeeper,
	fk types.FundingKeeper,
	rk types.RiskKeeper,
) Engine {
	return Engine{
		accountKeeper: ak,
		marketKeeper:  mk,
		fundingKeeper: fk,
		riskKeeper:    rk,
	}
}

// Fill is the input to Engine.Apply. It captures one perp match
// between a maker and a taker. Spot fills use a sibling SpotFill
// type defined in the parent package; the two are intentionally
// disjoint so callers cannot accidentally pass a perp-only field
// (ZeroPrice, LiquidationFee*, SkipMakerRiskCheck, ...) into the spot
// path or vice versa.
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

// Apply applies a perp fill to both maker and taker positions.
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
func (e Engine) Apply(ctx context.Context, f Fill) error {
	if err := e.fundingKeeper.SettlePositionFunding(ctx, f.MakerAccountIndex, f.MarketIndex); err != nil {
		return err
	}
	if err := e.fundingKeeper.SettlePositionFunding(ctx, f.TakerAccountIndex, f.MarketIndex); err != nil {
		return err
	}
	if !f.NoRiskCheck {
		if err := e.riskKeeper.SnapshotPreRisk(ctx, f.MakerAccountIndex); err != nil {
			return err
		}
		if err := e.riskKeeper.SnapshotPreRisk(ctx, f.TakerAccountIndex); err != nil {
			return err
		}
	}

	makerSign := int64(1)
	if !f.IsTakerAsk {
		makerSign = -1
	}
	takerSign := -makerSign

	makerRes, err := e.applyPositionChange(ctx, f.MakerAccountIndex, f.MarketIndex, f.Price, f.BaseAmount, makerSign)
	if err != nil {
		if errors.Is(err, errPositionOutOfBounds) {
			return sdkerrors.Wrapf(types.ErrMakerInvalidPosition,
				"account %d market %d", f.MakerAccountIndex, f.MarketIndex)
		}
		return err
	}
	takerRes, err := e.applyPositionChange(ctx, f.TakerAccountIndex, f.MarketIndex, f.Price, f.BaseAmount, takerSign)
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
		takerFee = types.FeeOf(notional, f.TakerFee)
		makerFee = types.FeeOf(notional, f.MakerFee)
	}

	// Liquidation improvement fee (lighter "improvement-over-zero-
	// price"): pre-compute once so we can hand the maker side a
	// single fee value AND know whether to credit the LLP / insurance
	// fund recipient at the end. Only chargeable when the fee is
	// configured AND fees are enabled on this fill.
	liqFee := math.ZeroInt()
	if !f.NoFee && f.LiquidationFeeBps > 0 {
		liqFee = liquidationImprovementFee(f, notional)
	}

	// Per-account dispatch: for each side, fold (PnL - fee) into the
	// right margin pool, debit the maker's improvement fee from the
	// same pool, and (isolated only) rebalance allocated_margin
	// against the new position's IM / market value. The dispatcher
	// in `applyAccount` routes to `applyIsolatedAccount` /
	// `applyCrossAccount` based on `res.Old.MarginMode`, keeping each
	// margin mode's full per-side pipeline cohesive in one file.
	//
	// The taker is never the improvement-fee victim, so it is
	// dispatched with `liqFee = 0`; the maker side carries the full
	// `liqFee` (still gated on `isMaker && liqFee > 0` inside the
	// per-mode handler).
	if err := e.applyAccount(ctx, &takerRes, takerFee, false /*isMaker*/, math.ZeroInt(), f); err != nil {
		return err
	}
	if err := e.applyAccount(ctx, &makerRes, makerFee, true /*isMaker*/, liqFee, f); err != nil {
		return err
	}

	// Global fee credits: treasury (taker + maker fees) and the
	// liquidation improvement fee recipient (LLP / insurance fund).
	// Both are credited to dedicated cross accounts that are disjoint
	// from the maker / taker, so it is safe to defer them until both
	// sides' per-account pipelines have completed.
	if !f.NoFee {
		treasuryFee := takerFee.Add(makerFee)
		if !treasuryFee.IsZero() {
			if err := e.accountKeeper.AddCollateral(ctx, perptypes.TreasuryAccountIndex, treasuryFee); err != nil {
				return err
			}
		}
		if liqFee.IsPositive() {
			recipient := f.LiquidationFeeRecipient
			if recipient == 0 {
				recipient = perptypes.InsuranceFundOperatorAccountIdx
			}
			if err := e.accountKeeper.AddCollateral(ctx, recipient, liqFee); err != nil {
				return err
			}
		}
	}

	// Open interest = sum over accounts of |position|, divided by 2 since
	// every fill touches exactly two accounts. Using the |newSize|-|oldSize|
	// delta ensures round-trips (open then close) return OI to its original
	// value rather than linearly growing with cumulative fill volume.
	oiDelta := (makerRes.OIDelta + takerRes.OIDelta) / 2
	if err := e.marketKeeper.UpdateOpenInterest(ctx, f.MarketIndex, oiDelta); err != nil {
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
		ok, err := e.riskKeeper.IsValidRiskChange(ctx, idx)
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

// applyAccount is the per-side dispatcher: it routes one side of a
// fill into the margin-mode-specific pipeline that will (a) fold the
// realized PnL net of fees into the right pool, (b) debit the maker
// liquidation improvement fee from the same pool when applicable, and
// (c) for isolated positions, rebalance `allocated_margin` against
// the new position's IM / market value (lighter
// `calculate_isolated_margin_change`).
//
// The dispatch is on `res.Old.MarginMode` — lighter parity: the
// pre-trade margin mode dictates how the trade flows. A position
// that opens fresh in this fill carries `Old.MarginMode == 0`
// (default cross) so cross routing applies, matching lighter's
// `is_*_position_isolated` short-circuit.
//
// Future `UnifiedMargin` mode plugs in here as a third case without
// disturbing either the cross or the isolated leg.
func (e Engine) applyAccount(ctx context.Context, res *positionChangeResult, fee math.Int, isMaker bool, liqFee math.Int, f Fill) error {
	switch res.Old.MarginMode {
	case perptypes.IsolatedMargin:
		return e.applyIsolatedAccount(ctx, res, fee, isMaker, liqFee, f)
	// case perptypes.UnifiedMargin:
	//     return e.applyUnifiedAccount(ctx, res, fee, isMaker, liqFee, f)
	default:
		return e.applyCrossAccount(ctx, res, fee, isMaker, liqFee)
	}
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
	rawFee := types.FeeOf(improvement, f.LiquidationFeeBps)
	cap1pct := notional.Quo(math.NewInt(100))
	if rawFee.GT(cap1pct) {
		rawFee = cap1pct
	}
	return rawFee
}
