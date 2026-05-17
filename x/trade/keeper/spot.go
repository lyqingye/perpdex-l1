package keeper

import (
	"context"
	"errors"

	sdkerrors "cosmossdk.io/errors"
	"cosmossdk.io/math"

	perptypes "github.com/perpdex/perpdex-l1/types"
	accounttypes "github.com/perpdex/perpdex-l1/x/account/types"
	"github.com/perpdex/perpdex-l1/x/trade/types"
)

// SpotFill is one spot match. Intentionally a strict subset of
// PerpFill so perp-only fields cannot leak across at compile time.
type SpotFill struct {
	MakerAccountIndex uint64
	TakerAccountIndex uint64
	MarketIndex       uint32
	Price             uint32
	BaseAmount        uint64
	IsTakerAsk        bool
	TakerFee          uint32
	MakerFee          uint32
	NoFee             bool
}

// ApplySpotMatching applies a spot fill (taker buys / sells base).
// Maker drains locked balance first (lock-on-place from x/orderbook),
// taker debits available directly. Insufficient-balance errors are
// wrapped into Maker* / Taker* sentinels for the matching loop.
func (k Keeper) ApplySpotMatching(ctx context.Context, f SpotFill, baseAssetID, quoteAssetID uint32) error {
	notional := math.NewIntFromUint64(f.BaseAmount).Mul(math.NewIntFromUint64(uint64(f.Price)))
	baseAmt := math.NewIntFromUint64(f.BaseAmount)
	if f.IsTakerAsk {
		// taker sells base: maker owes quote (locked), taker owes base.
		if err := k.spotMakerDebit(ctx, f.MakerAccountIndex, f.TakerAccountIndex, quoteAssetID, notional); err != nil {
			return err
		}
		if err := k.spotTakerDebit(ctx, f.TakerAccountIndex, f.MakerAccountIndex, baseAssetID, baseAmt); err != nil {
			return err
		}
	} else {
		// taker buys base: maker owes base (locked), taker owes quote.
		if err := k.spotMakerDebit(ctx, f.MakerAccountIndex, f.TakerAccountIndex, baseAssetID, baseAmt); err != nil {
			return err
		}
		if err := k.spotTakerDebit(ctx, f.TakerAccountIndex, f.MakerAccountIndex, quoteAssetID, notional); err != nil {
			return err
		}
	}

	if !f.NoFee {
		takerFee := types.FeeOf(notional, f.TakerFee)
		makerFee := types.FeeOf(notional, f.MakerFee)
		if takerFee.IsPositive() {
			if err := k.spotTakerDebit(ctx, f.TakerAccountIndex, perptypes.TreasuryAccountIndex, quoteAssetID, takerFee); err != nil {
				return err
			}
		}
		if makerFee.IsPositive() {
			// Maker fee comes from available balance: the lock only
			// covered notional, not fees.
			if err := k.spotMakerDebit(ctx, f.MakerAccountIndex, perptypes.TreasuryAccountIndex, quoteAssetID, makerFee); err != nil {
				return err
			}
		}
	}
	return nil
}

// spotMakerDebit moves amount from a maker, draining locked balance
// first and falling back to available as a defensive guard. Wraps
// insufficient-balance errors into ErrMakerInsufficientBalance.
func (k Keeper) spotMakerDebit(ctx context.Context, from, to uint64, assetID uint32, amount math.Int) error {
	if amount.IsNegative() {
		return types.ErrInvalidTransferAmount
	}
	if err := k.accountKeeper.TransferAccountAssetBalance(ctx, from, to, assetID, amount, true /* drainLockedFirst */); err != nil {
		if errors.Is(err, accounttypes.ErrInsufficientFunds) {
			return sdkerrors.Wrapf(types.ErrMakerInsufficientBalance, "%s", err.Error())
		}
		return err
	}
	return nil
}

// spotTakerDebit moves amount from a taker against available balance
// (takers do not lock-on-place). Wraps insufficient-balance errors
// into ErrTakerInsufficientBalance.
func (k Keeper) spotTakerDebit(ctx context.Context, from, to uint64, assetID uint32, amount math.Int) error {
	if amount.IsNegative() {
		return types.ErrInvalidTransferAmount
	}
	if err := k.accountKeeper.TransferAccountAssetBalance(ctx, from, to, assetID, amount, false /* drainLockedFirst */); err != nil {
		if errors.Is(err, accounttypes.ErrInsufficientFunds) {
			return sdkerrors.Wrapf(types.ErrTakerInsufficientBalance, "%s", err.Error())
		}
		return err
	}
	return nil
}
