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
		takerFee = notional.Mul(math.NewIntFromUint64(uint64(f.TakerFee))).Quo(math.NewInt(int64(perptypes.FeeTick)))
		makerFee = notional.Mul(math.NewIntFromUint64(uint64(f.MakerFee))).Quo(math.NewInt(int64(perptypes.FeeTick)))
	}

	// Route realized PnL + fee to the right pool (allocated_margin
	// for isolated positions, cross collateral for cross). This
	// mirrors lighter's `taker_collateral_delta` flowing into
	// `allocated_margin` for isolated and into cross collateral for
	// cross before the margin_delta auto-allocation step.
	if err := e.applyPositionFinancials(ctx, &takerRes, takerFee); err != nil {
		return err
	}
	if err := e.applyPositionFinancials(ctx, &makerRes, makerFee); err != nil {
		return err
	}

	// Treasury fee credit (sum of both sides). Treasury is the
	// `TreasuryAccountIndex` cross account regardless of whether
	// either side is isolated — the fee flows to a global pool.
	if !f.NoFee {
		treasuryFee := takerFee.Add(makerFee)
		if !treasuryFee.IsZero() {
			if err := e.accountKeeper.AddCollateral(ctx, perptypes.TreasuryAccountIndex, treasuryFee); err != nil {
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
				if err := e.debitFromMarginPool(ctx, &makerRes, liqFee); err != nil {
					return err
				}
				recipient := f.LiquidationFeeRecipient
				if recipient == 0 {
					recipient = perptypes.InsuranceFundOperatorAccountIdx
				}
				if err := e.accountKeeper.AddCollateral(ctx, recipient, liqFee); err != nil {
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
	if err := e.applyIsolatedMargin(ctx, &takerRes, takerFee, false /*isMaker*/, f); err != nil {
		return err
	}
	if err := e.applyIsolatedMargin(ctx, &makerRes, makerFee, true /*isMaker*/, f); err != nil {
		return err
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

// applyPositionFinancials routes (`realized_pnl - fee`) to the right
// pool: into `allocated_margin` for an isolated position (lighter:
// `is_*_position_isolated` branch), or into cross collateral
// otherwise. The mode-specific writes live in `isolated.go` /
// `cross.go`; this dispatcher keeps the routing decision in one
// place, ready for a future `unified.go` to plug in a third branch.
//
// `fee` is the per-side debit owed to the treasury (already non-
// negative in the caller). When the position closes (`new size == 0`
// for an isolated position) the realized PnL still flows through
// allocated_margin first; the subsequent `applyIsolatedMargin` step
// then releases everything back to cross via a negative margin_delta.
func (e Engine) applyPositionFinancials(ctx context.Context, res *positionChangeResult, fee math.Int) error {
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
	switch res.Old.MarginMode {
	case perptypes.IsolatedMargin:
		return e.isolatedAddAllocatedMargin(ctx, res, delta)
	default:
		// Cross (and any future unified mode falling back to cross
		// behaviour) lands here.
		return e.crossAddCollateral(ctx, res.AccountIdx, delta)
	}
}

// debitFromMarginPool subtracts `amount` from the side's effective
// margin pool: `allocated_margin` for an isolated position, cross
// collateral otherwise. Used by the liquidation improvement-fee path
// so an isolated victim's cross account is not arbitrarily disturbed.
// Mirrors the same dispatcher shape as `applyPositionFinancials`.
func (e Engine) debitFromMarginPool(ctx context.Context, res *positionChangeResult, amount math.Int) error {
	if amount.IsZero() {
		return nil
	}
	switch res.Old.MarginMode {
	case perptypes.IsolatedMargin:
		return e.isolatedDebit(ctx, res, amount)
	default:
		return e.crossDebit(ctx, res.AccountIdx, amount)
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
	rawFee := improvement.Mul(math.NewIntFromUint64(uint64(f.LiquidationFeeBps))).Quo(math.NewInt(int64(perptypes.FeeTick)))
	cap1pct := notional.Quo(math.NewInt(100))
	if rawFee.GT(cap1pct) {
		rawFee = cap1pct
	}
	return rawFee
}
