package keeper

import (
	"context"
	"errors"

	"cosmossdk.io/collections"
	"cosmossdk.io/math"

	sdk "github.com/cosmos/cosmos-sdk/types"

	perptypes "github.com/perpdex/perpdex-l1/types"
	"github.com/perpdex/perpdex-l1/x/account/types"
)

// setPosition is the package-private write primitive for the
// AccountPosition row. Cohesive mutators (UpdatePosition,
// SetPositionLeverage) funnel through here so position state has a
// single choke point for event emission. Every successful write fires
// an `EventPositionUpdated` typed event carrying the full post-write
// row snapshot, so off-chain consumers can mirror AccountPositions
// without polling state — this matters because positions are also
// mutated by cross-module callers (x/trade fills, x/funding
// settlement, x/liquidation / x/risk) outside of x/account's msg
// server.
func (k Keeper) setPosition(ctx context.Context, p types.AccountPosition) error {
	if err := k.AccountPositions.Set(ctx, collections.Join(p.AccountIndex, p.MarketIndex), p); err != nil {
		return err
	}
	return sdk.UnwrapSDKContext(ctx).EventManager().EmitTypedEvent(&types.EventPositionUpdated{
		Position: p,
	})
}

// GetPosition returns the position; an empty zero-valued one if absent.
func (k Keeper) GetPosition(ctx context.Context, accIdx uint64, marketIdx uint32) (types.AccountPosition, error) {
	p, err := k.AccountPositions.Get(ctx, collections.Join(accIdx, marketIdx))
	if err != nil {
		if errors.Is(err, collections.ErrNotFound) {
			return types.AccountPosition{
				AccountIndex:             accIdx,
				MarketIndex:              marketIdx,
				BaseSize:                 math.ZeroInt(),
				EntryQuote:               math.ZeroInt(),
				LastFundingRatePrefixSum: math.ZeroInt(),
				AllocatedMargin:          math.ZeroInt(),
				MarginMode:               perptypes.CrossMargin,
			}, nil
		}
		return types.AccountPosition{}, err
	}
	p.NormalizeIntFields()
	return p, nil
}

// UpdatePosition is the canonical read-modify-write wrapper for
// `AccountPosition`. It loads the position (auto-vivifying a zero-
// valued record when missing — same semantics as `GetPosition`),
// runs the supplied `mut` callback against a mutable pointer, then
// persists the result through the package-private `setPosition`.
//
// Cross-module callers (x/trade, x/funding, x/liquidation) own their
// mutation logic but MUST go through this entry point so x/account
// can attach invariants / events / metrics in exactly one place.
//
// If `mut` returns an error the position is NOT persisted (so a
// caller can short-circuit on bounds violations like
// `errPositionOutOfBounds`). The returned `AccountPosition` is the
// post-mutation value.
func (k Keeper) UpdatePosition(
	ctx context.Context,
	accIdx uint64,
	marketIdx uint32,
	mut func(*types.AccountPosition) error,
) (types.AccountPosition, error) {
	pos, err := k.GetPosition(ctx, accIdx, marketIdx)
	if err != nil {
		return types.AccountPosition{}, err
	}
	pos.AccountIndex = accIdx
	pos.MarketIndex = marketIdx
	if err := mut(&pos); err != nil {
		return types.AccountPosition{}, err
	}
	if err := k.setPosition(ctx, pos); err != nil {
		return types.AccountPosition{}, err
	}
	return pos, nil
}

// SetPositionLeverage flips a position's `MarginMode` and
// `InitialMarginFraction`. Used by Msg.UpdateLeverage; the caller
// has already validated the position is empty and the imf falls
// inside [market_min, MarginTick].
func (k Keeper) SetPositionLeverage(
	ctx context.Context,
	accIdx uint64,
	marketIdx uint32,
	marginMode uint32,
	imf uint32,
) error {
	_, err := k.UpdatePosition(ctx, accIdx, marketIdx, func(p *types.AccountPosition) error {
		p.MarginMode = marginMode
		p.InitialMarginFraction = imf
		return nil
	})
	return err
}

// IterateAccountPositions walks every persisted AccountPosition row
// owned by `accountIdx`. The callback returns `true` to stop early.
//
// Per-account driver for risk / liquidation / funding loops
// (ComputeCrossRisk, IsValidRiskChangeFrom, SnapshotRisk,
// IterateIsolatedPositions, processAccount, rankVictimPositionsByUPnL,
// settleAllPositionFunding) so they touch only persisted rows instead
// of scanning the full MaxPerpsMarketIndex range.
//
// Callers may still see Position == 0 rows (the keeper does not delete
// positions when they net to zero, only when funding is settled and the
// position is closed); skip them with `pos.BaseSize.IsZero()` if the
// caller cares about non-empty positions only.
func (k Keeper) IterateAccountPositions(
	ctx context.Context,
	accountIdx uint64,
	cb func(types.AccountPosition) bool,
) error {
	rng := collections.NewPrefixedPairRange[uint64, uint32](accountIdx)
	iter, err := k.AccountPositions.Iterate(ctx, rng)
	if err != nil {
		return err
	}
	defer iter.Close()
	for ; iter.Valid(); iter.Next() {
		p, err := iter.Value()
		if err != nil {
			return err
		}
		p.NormalizeIntFields()
		if cb(p) {
			return nil
		}
	}
	return nil
}
