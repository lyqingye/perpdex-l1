package perp

import (
	"context"
	"errors"

	sdkerrors "cosmossdk.io/errors"
	"cosmossdk.io/math"

	perptypes "github.com/perpdex/perpdex-l1/types"
	accounttypes "github.com/perpdex/perpdex-l1/x/account/types"
	risktypes "github.com/perpdex/perpdex-l1/x/risk/types"
	"github.com/perpdex/perpdex-l1/x/trade/types"
)

// Engine encapsulates the perp trade-application pipeline. Stateless;
// the surrounding x/trade keeper composes it and forwards
// ApplyPerpsMatching calls into Apply. Per-margin-mode logic
// (cross / isolated / future unified) lives in sibling files.
type Engine struct {
	accountKeeper types.AccountKeeper
	marketKeeper  types.MarketKeeper
	fundingKeeper types.FundingKeeper
	riskKeeper    types.RiskKeeper
}

// NewEngine wires the engine with its required keepers. Pure
// constructor: no I/O, no schema work.
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

// Fill is one perp match between a maker and a taker. Spot uses a
// disjoint SpotFill in the parent package so perp-only fields
// (ZeroPrice, LiquidationFee*, Skip*RiskCheck, ...) cannot leak across.
type Fill struct {
	MakerAccountIndex uint64
	TakerAccountIndex uint64
	MarketIndex       uint32
	Price             uint32
	BaseAmount        uint64
	IsTakerAsk        bool
	TakerFee          uint32
	MakerFee          uint32
	NoFee             bool // liquidation / deleverage path
	// NoRiskCheck skips post-trade IsValidRiskChangeFrom on both sides
	// (and the matching pre-fill SnapshotRisk). Reserved for forced
	// close-outs (market-expiry exit, IF/ADL absorption); prefer
	// Skip{Maker,Taker}RiskCheck for one-sided suppression.
	NoRiskCheck bool
	// SkipMakerRiskCheck suppresses only the maker-side post-trade
	// risk check. Used when the maker is the victim being closed and
	// any non-HEALTHY post-state would otherwise spuriously reject.
	SkipMakerRiskCheck bool
	// SkipTakerRiskCheck suppresses only the taker-side post-trade
	// risk check. Used by Deleverage so LLP / IF absorbers can take
	// on residual exposure; user-ADL keeps this off for
	// defense-in-depth on the counterparty.
	SkipTakerRiskCheck bool
	// ZeroPrice + LiquidationFeeBps + LiquidationFeeRecipient describe
	// the "improvement-over-zero-price" liquidation fee:
	//
	//   price_diff_rate = (|Price - ZeroPrice| * FeeTick) / Price
	//   effective_rate  = min(LiquidationFeeBps, price_diff_rate)
	//   fee             = notional * effective_rate / FeeTick
	//
	// Improvement direction follows taker side: ask (victim long)
	// needs Price > ZeroPrice; bid (victim short) needs Price <
	// ZeroPrice. Non-improving or floor-equal fills produce fee=0.
	// Caller MUST set Maker/TakerFee to 0 — disjoint fee paths.
	// Fee is debited from the victim and credited to
	// LiquidationFeeRecipient (LLP / Insurance Fund).
	ZeroPrice               uint32
	LiquidationFeeBps       uint32
	LiquidationFeeRecipient uint64
}

// Apply applies a perp fill to both maker and taker. 8-step pipeline:
//  1. settle pending funding (both sides)
//  2. snapshot pre-state risk
//  3. update positions + bounds-check |position| / |entry_quote|
//  4. route realized PnL (isolated→allocated_margin, cross→collateral)
//  5. charge maker/taker/treasury fees and liquidation improvement fee
//  6. for isolated, auto-allocate margin_delta from cross collateral
//  7. update OI = (|maker_delta| + |taker_delta|) / 2
//  8. validate IsValidRiskChangeFrom on both sides
//
// Per-side failures are wrapped into Maker* / Taker* sentinels so the
// matching loop can evict a bad maker and continue, or stop a bad
// taker while preserving prior fills.
func (e Engine) Apply(ctx context.Context, f Fill) error {
	if err := e.fundingKeeper.SettlePositionFunding(ctx, f.MakerAccountIndex, f.MarketIndex); err != nil {
		return err
	}
	if err := e.fundingKeeper.SettlePositionFunding(ctx, f.TakerAccountIndex, f.MarketIndex); err != nil {
		return err
	}
	// Snapshot only the sides that will actually enforce
	// IsValidRiskChangeFrom downstream; pre-state stays function-local
	// so it cannot leak into a sibling fill.
	var (
		makerPre risktypes.PreRiskSnapshot
		takerPre risktypes.PreRiskSnapshot
	)
	if !f.NoRiskCheck {
		if !f.SkipMakerRiskCheck {
			pre, err := e.riskKeeper.SnapshotRisk(ctx, f.MakerAccountIndex)
			if err != nil {
				return err
			}
			makerPre = pre
		}
		if !f.SkipTakerRiskCheck {
			pre, err := e.riskKeeper.SnapshotRisk(ctx, f.TakerAccountIndex)
			if err != nil {
				return err
			}
			takerPre = pre
		}
	}

	// Derive the per-side signed base delta from the fill direction.
	// IsTakerAsk == true means the taker is on the ask (selling base),
	// so the maker buys base (+) and the taker sells (-); the inverse
	// holds when the taker is bidding.
	makerDelta := math.NewIntFromUint64(f.BaseAmount)
	if !f.IsTakerAsk {
		makerDelta = makerDelta.Neg()
	}
	takerDelta := makerDelta.Neg()

	// Snapshot the market's current FundingRatePrefixSum once and
	// thread it into both ApplyFill calls so x/account doesn't need
	// its own marketKeeper handle in this hot path (Cosmos late-bound
	// keepers aren't visible to the trade engine's accountKeeper
	// interface copy). ApplyFill uses it only on the OPEN / FLIP
	// transitions to seed the first post-open funding boundary.
	md, err := e.marketKeeper.GetMarketDetails(ctx, f.MarketIndex)
	if err != nil {
		return err
	}
	fundingPrefix := md.FundingRatePrefixSum

	// ApplyFill is the cohesive entry-point on x/account.Keeper (issue
	// #91): it classifies the transition (open / mutate / close /
	// flip), persists through the matching package-private primitive,
	// emits exactly one (or two, for flip) lifecycle event(s) and
	// returns the pre/post snapshots + realized PnL + OI delta the
	// pipeline below keys off. Out-of-bounds positions surface as
	// ErrPositionOutOfBounds — wrapped per-side so the matching loop
	// can evict a bad maker / stop a bad taker.
	makerRes, err := e.accountKeeper.ApplyFill(ctx, f.MakerAccountIndex, f.MarketIndex, f.Price, makerDelta, fundingPrefix)
	if err != nil {
		if errors.Is(err, accounttypes.ErrPositionOutOfBounds) {
			return sdkerrors.Wrapf(types.ErrMakerInvalidPosition,
				"account %d market %d", f.MakerAccountIndex, f.MarketIndex)
		}
		return err
	}
	takerRes, err := e.accountKeeper.ApplyFill(ctx, f.TakerAccountIndex, f.MarketIndex, f.Price, takerDelta, fundingPrefix)
	if err != nil {
		if errors.Is(err, accounttypes.ErrPositionOutOfBounds) {
			return sdkerrors.Wrapf(types.ErrTakerInvalidPosition,
				"account %d market %d", f.TakerAccountIndex, f.MarketIndex)
		}
		return err
	}

	// Compute fees once: the same value feeds both the per-side debit
	// and the isolated margin_delta calculation (`trade_pnl - fee`).
	notional := math.NewIntFromUint64(f.BaseAmount).Mul(math.NewIntFromUint64(uint64(f.Price)))
	var takerFee, makerFee math.Int
	if f.NoFee {
		takerFee = math.ZeroInt()
		makerFee = math.ZeroInt()
	} else {
		takerFee = types.FeeOf(notional, f.TakerFee)
		makerFee = types.FeeOf(notional, f.MakerFee)
	}

	// Liquidation improvement fee: pre-compute once so the maker debit
	// and the end-of-fn recipient credit see the same value.
	liqFee := math.ZeroInt()
	if !f.NoFee && f.LiquidationFeeBps > 0 {
		liqFee = liquidationImprovementFee(f, notional)
	}

	// Per-account dispatch: fold (PnL - fee) into the right margin pool
	// and (isolated only) rebalance allocated_margin. The taker never
	// pays the improvement fee, so it is dispatched with liqFee = 0;
	// only the maker carries the full liqFee.
	if err := e.applyAccount(ctx, &takerRes, takerFee, false /*isMaker*/, math.ZeroInt(), f); err != nil {
		return err
	}
	if err := e.applyAccount(ctx, &makerRes, makerFee, true /*isMaker*/, liqFee, f); err != nil {
		return err
	}

	// Global fee credits to treasury and improvement-fee recipient.
	// Both target accounts are disjoint from maker / taker, so they
	// can be deferred until both per-side pipelines have completed.
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

	// OI = sum of |position|, divided by 2 since each fill touches two
	// accounts. Using |new|-|old| keeps round-trips OI-neutral instead
	// of growing linearly with cumulative fill volume.
	oiDelta := (makerRes.OIDelta + takerRes.OIDelta) / 2
	if err := e.marketKeeper.UpdateOpenInterest(ctx, f.MarketIndex, oiDelta); err != nil {
		return err
	}

	if f.NoRiskCheck {
		return nil
	}
	// Both sides must pass the post-state risk check: skipping the
	// maker would let a low-collateral resting order close into a
	// fresh unhealthy position. Per-side Skip* flags carve out the
	// "victim is maker" and "LLP/IF absorber" exceptions.
	for _, side := range []struct {
		idx uint64
		pre risktypes.PreRiskSnapshot
	}{
		{f.TakerAccountIndex, takerPre},
		{f.MakerAccountIndex, makerPre},
	} {
		if f.SkipMakerRiskCheck && side.idx == f.MakerAccountIndex {
			continue
		}
		if f.SkipTakerRiskCheck && side.idx == f.TakerAccountIndex {
			continue
		}
		ok, err := e.riskKeeper.IsValidRiskChangeFrom(ctx, side.idx, side.pre)
		if err != nil {
			return err
		}
		if !ok {
			// Classify by side so the matching loop can evict a bad
			// maker and continue, or stop a bad taker while keeping
			// prior fills.
			if side.idx == f.MakerAccountIndex {
				return sdkerrors.Wrapf(types.ErrMakerRiskRegression,
					"account %d", side.idx)
			}
			return sdkerrors.Wrapf(types.ErrTakerRiskRegression,
				"account %d", side.idx)
		}
	}
	return nil
}

// applyAccount dispatches one side of a fill to the margin-mode
// pipeline (cross / isolated; future unified plugs in here). Dispatch
// keys on res.Old.MarginMode — a freshly opened position has
// Old.MarginMode == 0 (default cross), matching the
// `is_*_position_isolated` short-circuit.
func (e Engine) applyAccount(ctx context.Context, res *accounttypes.FillApplyResult, fee math.Int, isMaker bool, liqFee math.Int, f Fill) error {
	switch res.Old.MarginMode {
	case perptypes.IsolatedMargin:
		return e.applyIsolatedAccount(ctx, res, fee, isMaker, liqFee, f)
	// case perptypes.UnifiedMargin:
	//     return e.applyUnifiedAccount(ctx, res, fee, isMaker, liqFee, f)
	default:
		return e.applyCrossAccount(ctx, res, fee, isMaker, liqFee)
	}
}

// liquidationImprovementFee computes the improvement-over-zero-price
// liquidation fee:
//
//	improvement     = sign(takerSide) * (price - zeroPrice)
//	price_diff_rate = (|improvement| * FeeTick) / price
//	effective_rate  = min(LiquidationFeeBps, price_diff_rate)
//	fee             = notional * effective_rate / FeeTick
//
// taker side flips the improvement sign so a fill BETTER than zero
// price always yields a positive fee. Price == ZeroPrice yields 0
// (matches the keeper-driven IoC close-out at exactly zero price).
// Upper bound comes from min(LiquidationFeeBps, price_diff_rate); no
// hard-coded notional cap.
func liquidationImprovementFee(f Fill, notional math.Int) math.Int {
	if f.LiquidationFeeBps == 0 || f.BaseAmount == 0 || f.Price == 0 {
		return math.ZeroInt()
	}
	priceInt := math.NewIntFromUint64(uint64(f.Price))
	zpInt := math.NewIntFromUint64(uint64(f.ZeroPrice))
	var improvement math.Int
	if f.IsTakerAsk {
		// Victim long: higher fill price is better.
		improvement = priceInt.Sub(zpInt)
	} else {
		// Victim short: lower fill price is better.
		improvement = zpInt.Sub(priceInt)
	}
	if !improvement.IsPositive() {
		return math.ZeroInt()
	}
	feeTick := math.NewIntFromUint64(perptypes.FeeTick)
	priceDiffRate := improvement.Mul(feeTick).Quo(priceInt)
	feeBpsInt := math.NewIntFromUint64(uint64(f.LiquidationFeeBps))
	effectiveRate := priceDiffRate
	if feeBpsInt.LT(effectiveRate) {
		effectiveRate = feeBpsInt
	}
	if !effectiveRate.IsPositive() {
		return math.ZeroInt()
	}
	return notional.Mul(effectiveRate).Quo(feeTick)
}
