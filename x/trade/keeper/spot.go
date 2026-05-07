package keeper

import (
	"context"
	"errors"
	"fmt"

	sdkerrors "cosmossdk.io/errors"
	"cosmossdk.io/math"

	perptypes "github.com/perpdex/perpdex-l1/types"
	accounttypes "github.com/perpdex/perpdex-l1/x/account/types"
	"github.com/perpdex/perpdex-l1/x/trade/types"
)

// SpotFill is the input to ApplySpotMatching. It captures one spot
// match between a maker and a taker. Spot trades have no notion of
// position, isolated margin, or zero-price liquidation, so SpotFill is
// intentionally a strict subset of PerpFill — callers that try to pass
// perp-only fields here get a compile error rather than silently
// dropping them on the floor.
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
func (k Keeper) ApplySpotMatching(ctx context.Context, f SpotFill, baseAssetID, quoteAssetID uint32) error {
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
		takerFee := types.FeeOf(notional, f.TakerFee)
		makerFee := types.FeeOf(notional, f.MakerFee)
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
	if err := k.accountKeeper.TransferAccountAssetBalance(ctx, from, to, assetID, amount, true /* drainLockedFirst */); err != nil {
		if errors.Is(err, accounttypes.ErrInsufficientFunds) {
			return sdkerrors.Wrapf(types.ErrMakerInsufficientBalance, "%s", err.Error())
		}
		return err
	}
	return nil
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
	if err := k.accountKeeper.TransferAccountAssetBalance(ctx, from, to, assetID, amount, false /* drainLockedFirst */); err != nil {
		if errors.Is(err, accounttypes.ErrInsufficientFunds) {
			return sdkerrors.Wrapf(types.ErrTakerInsufficientBalance, "%s", err.Error())
		}
		return err
	}
	return nil
}
