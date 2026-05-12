package keeper

import (
	"context"
	"errors"
	"fmt"
	"math"
	"strconv"

	"cosmossdk.io/collections"
	"cosmossdk.io/core/store"

	"github.com/cosmos/cosmos-sdk/codec"
	sdk "github.com/cosmos/cosmos-sdk/types"

	perptypes "github.com/perpdex/perpdex-l1/types"
	"github.com/perpdex/perpdex-l1/x/market/types"
)

type Keeper struct {
	cdc          codec.BinaryCodec
	storeService store.KVStoreService
	authority    string

	assetKeeper       types.AssetKeeper
	liquidationKeeper types.LiquidationKeeper

	Schema        collections.Schema
	Params        collections.Item[types.Params]
	Markets       collections.Map[uint32, types.Market]
	MarketDetails collections.Map[uint32, types.MarketDetails]
	ExpiryIndex   collections.KeySet[collections.Pair[int64, uint32]]
}

func NewKeeper(cdc codec.BinaryCodec, storeService store.KVStoreService, authority string, assetK types.AssetKeeper) Keeper {
	sb := collections.NewSchemaBuilder(storeService)
	k := Keeper{
		cdc:          cdc,
		storeService: storeService,
		authority:    authority,
		assetKeeper:  assetK,

		Params:        collections.NewItem(sb, types.ParamsKey, "params", codec.CollValue[types.Params](cdc)),
		Markets:       collections.NewMap(sb, types.MarketKey, "markets", collections.Uint32Key, codec.CollValue[types.Market](cdc)),
		MarketDetails: collections.NewMap(sb, types.MarketDetailsKey, "market_details", collections.Uint32Key, codec.CollValue[types.MarketDetails](cdc)),
		ExpiryIndex:   collections.NewKeySet(sb, types.ExpiryIndexKey, "expiry_index", collections.PairKeyCodec(collections.Int64Key, collections.Uint32Key)),
	}
	schema, err := sb.Build()
	if err != nil {
		panic(fmt.Errorf("market: %w", err))
	}
	k.Schema = schema
	return k
}

// SetLiquidationKeeper plugs in the liquidation hook used by
// expireMarket / EndBlocker to close out positions against the
// insurance fund when a market transitions to EXPIRED. The setter
// exists because liquidation depends on market (mark price lookups)
// and market depends on liquidation (exit position) — they cannot be
// constructor-wired without an import cycle. When this is left unset
// (e.g. in keeper-level tests), the expireMarket helper still flips
// the market status but logs the missing-hook condition and emits
// EventTypeMarketExpireExitFailed so monitors can detect the gap.
func (k *Keeper) SetLiquidationKeeper(l types.LiquidationKeeper) { k.liquidationKeeper = l }

func (k Keeper) Authority() string { return k.authority }

// GetMarket returns a Market or ErrMarketNotFound.
func (k Keeper) GetMarket(ctx context.Context, idx uint32) (types.Market, error) {
	m, err := k.Markets.Get(ctx, idx)
	if err != nil {
		if errors.Is(err, collections.ErrNotFound) {
			return types.Market{}, types.ErrMarketNotFound.Wrapf("market_index=%d", idx)
		}
		return types.Market{}, err
	}
	return m, nil
}

// GetMarketDetails returns a MarketDetails or ErrMarketNotFound. Read
// is a pure pass-through: every writer (SetMarketDetails / Genesis)
// normalises math.Int fields so reads never see nil values.
func (k Keeper) GetMarketDetails(ctx context.Context, idx uint32) (types.MarketDetails, error) {
	d, err := k.MarketDetails.Get(ctx, idx)
	if err != nil {
		if errors.Is(err, collections.ErrNotFound) {
			return types.MarketDetails{}, types.ErrMarketNotFound.Wrapf("market_details_index=%d", idx)
		}
		return types.MarketDetails{}, err
	}
	return d, nil
}

// SetMarket is a thin wrapper around setMarketWithIndex that does NOT
// maintain ExpiryIndex deltas — only safe for Init / Create paths
// where there is no prior market record on disk. Update paths must use
// setMarketWithIndex with the old record so the secondary index stays
// in sync.
func (k Keeper) SetMarket(ctx context.Context, m types.Market) error {
	return k.setMarketWithIndex(ctx, nil, m)
}

// setMarketWithIndex persists `m` and atomically maintains the
// ExpiryIndex secondary index:
//   - If `old` is non-nil and its expiry differed from `m`, remove the
//     stale (oldExpiry, marketIdx) entry first.
//   - If `m.ExpiryTimestamp > 0`, register the new (newExpiry,
//     marketIdx) entry.
//
// This is the single write-path for Market; callers MUST go through it
// (directly or via SetMarket) so the index never drifts. Errors are
// propagated (no `_ = ...`) so KV failures surface to the handler.
func (k Keeper) setMarketWithIndex(ctx context.Context, old *types.Market, m types.Market) error {
	if err := k.Markets.Set(ctx, m.MarketIndex, m); err != nil {
		return err
	}
	if old != nil && old.ExpiryTimestamp > 0 && old.ExpiryTimestamp != m.ExpiryTimestamp {
		if err := k.ExpiryIndex.Remove(ctx, collections.Join(old.ExpiryTimestamp, old.MarketIndex)); err != nil {
			return err
		}
	}
	if m.ExpiryTimestamp > 0 {
		if err := k.ExpiryIndex.Set(ctx, collections.Join(m.ExpiryTimestamp, m.MarketIndex)); err != nil {
			return err
		}
	}
	return nil
}

// SetMarketDetails persists the MarketDetails after normalising the
// math.Int fields. Read paths can rely on every persisted record
// having non-nil math.Int values.
func (k Keeper) SetMarketDetails(ctx context.Context, d types.MarketDetails) error {
	d.NormalizeIntFields()
	return k.MarketDetails.Set(ctx, d.MarketIndex, d)
}

// expireMarket is the single code path for flipping a market into
// MarketStatusExpired. It MUST be used by both EndBlocker (auto
// expiry on `now >= ExpiryTimestamp`) and any future governance path
// that wants to delist a market. It:
//
//  1. Writes `m` back with Status=EXPIRED through setMarketWithIndex
//     (which drops the secondary index entry because the new expiry
//     equals the old one and we then explicitly Remove it below).
//  2. Removes the ExpiryIndex entry so the market is not reprocessed
//     in subsequent EndBlocker passes.
//  3. Invokes liquidationKeeper.ApplyExitPosition to close residual
//     positions against the insurance fund. nil-safe: a missing
//     LiquidationKeeper is treated as "no exit handler wired" and
//     surfaced through EventTypeMarketExpireExitFailed so it is
//     monitorable rather than a silent panic.
//  4. Emits EventTypeMarketExpired regardless of whether the exit
//     hook succeeded — the market is expired and trading must stop.
func (k Keeper) expireMarket(ctx context.Context, m types.Market) error {
	sdkCtx := sdk.UnwrapSDKContext(ctx)
	m.Status = perptypes.MarketStatusExpired
	if err := k.Markets.Set(ctx, m.MarketIndex, m); err != nil {
		return err
	}
	if m.ExpiryTimestamp > 0 {
		if err := k.ExpiryIndex.Remove(ctx, collections.Join(m.ExpiryTimestamp, m.MarketIndex)); err != nil {
			// Index drift is non-fatal: the secondary index is
			// best-effort. We still log so it shows up in
			// post-mortems.
			sdkCtx.Logger().Error("market: failed to remove expiry index entry",
				"market", m.MarketIndex, "err", err)
		}
	}
	exitErr := error(nil)
	if k.liquidationKeeper == nil {
		exitErr = errors.New("liquidation keeper not wired")
	} else if err := k.liquidationKeeper.ApplyExitPosition(ctx, m.MarketIndex); err != nil {
		exitErr = err
	}
	if exitErr != nil {
		sdkCtx.Logger().Error("market: apply exit failed",
			"market", m.MarketIndex, "err", exitErr)
		sdkCtx.EventManager().EmitEvent(sdk.NewEvent(
			types.EventTypeMarketExpireExitFailed,
			sdk.NewAttribute(types.AttributeKeyMarketIndex, strconv.FormatUint(uint64(m.MarketIndex), 10)),
			sdk.NewAttribute(types.AttributeKeyExitError, exitErr.Error()),
		))
	}
	sdkCtx.EventManager().EmitEvent(sdk.NewEvent(
		types.EventTypeMarketExpired,
		sdk.NewAttribute(types.AttributeKeyMarketIndex, strconv.FormatUint(uint64(m.MarketIndex), 10)),
	))
	return nil
}

// AllocateNonce returns the next ask or bid nonce for the market and
// persists the new value. ask increments, bid decrements; the call
// returns ErrNonceExhausted if ask>=bid would result.
func (k Keeper) AllocateNonce(ctx context.Context, marketIdx uint32, isAsk bool) (int64, error) {
	d, err := k.GetMarketDetails(ctx, marketIdx)
	if err != nil {
		return 0, err
	}
	if d.AskNonce >= d.BidNonce {
		return 0, types.ErrNonceExhausted
	}
	var nonce int64
	if isAsk {
		nonce = d.AskNonce
		d.AskNonce++
	} else {
		nonce = d.BidNonce
		d.BidNonce--
	}
	if d.AskNonce >= d.BidNonce {
		return 0, types.ErrNonceExhausted
	}
	if err := k.SetMarketDetails(ctx, d); err != nil {
		return 0, err
	}
	return nonce, nil
}

// UpdateOpenInterest applies a signed delta to a market's open
// interest. Uses checked-add semantics so a buggy caller cannot
// silently underflow into negative territory or overflow int64.
//   - Underflow (`OpenInterest + delta < 0`): rejected. Tracking OI
//     below zero would mask accounting bugs in the trade engine; we
//     prefer a loud error here over silent clamping.
//   - Overflow: rejected via the same checked-add path.
//   - When the resulting OI exceeds the per-market
//     `OpenInterestLimit`, returns ErrOpenInterestLimit without
//     persisting.
func (k Keeper) UpdateOpenInterest(ctx context.Context, marketIdx uint32, delta int64) error {
	d, err := k.GetMarketDetails(ctx, marketIdx)
	if err != nil {
		return err
	}
	next, err := checkedAddInt64(d.OpenInterest, delta)
	if err != nil {
		return err
	}
	if next < 0 {
		return types.ErrInvalidParams.Wrapf(
			"open_interest delta=%d would drive oi=%d below zero (current=%d)",
			delta, next, d.OpenInterest,
		)
	}
	if d.OpenInterestLimit > 0 && uint64(next) > d.OpenInterestLimit {
		return types.ErrOpenInterestLimit
	}
	d.OpenInterest = next
	return k.SetMarketDetails(ctx, d)
}

// checkedAddInt64 returns a+b or an error when the addition would
// overflow int64 in either direction.
func checkedAddInt64(a, b int64) (int64, error) {
	if b > 0 && a > math.MaxInt64-b {
		return 0, types.ErrInvalidParams.Wrapf("int64 overflow: %d + %d", a, b)
	}
	if b < 0 && a < math.MinInt64-b {
		return 0, types.ErrInvalidParams.Wrapf("int64 underflow: %d + %d", a, b)
	}
	return a + b, nil
}

// IsPerpsMarket reports whether the market index falls in the
// perpetual range defined by the current chain Params.
func (k Keeper) IsPerpsMarket(ctx context.Context, idx uint32) bool {
	p, err := k.Params.Get(ctx)
	if err != nil {
		// Fall back to the chain-wide constants if Params are
		// unavailable (e.g. very early in app boot before
		// InitGenesis). This matches the pre-refactor behaviour.
		return idx <= perptypes.MaxPerpsMarketIndex && idx != perptypes.NilMarketIndex
	}
	return p.IsPerpsIndex(idx)
}

// IsSpotMarket reports whether the market index falls in the spot
// range defined by the current chain Params.
func (k Keeper) IsSpotMarket(ctx context.Context, idx uint32) bool {
	p, err := k.Params.Get(ctx)
	if err != nil {
		return idx >= perptypes.MinSpotMarketIndex && idx <= perptypes.MaxSpotMarketIndex
	}
	return p.IsSpotIndex(idx)
}

// IterateMarkets walks all markets in index order.
func (k Keeper) IterateMarkets(ctx context.Context, cb func(types.Market) bool) error {
	iter, err := k.Markets.Iterate(ctx, nil)
	if err != nil {
		return err
	}
	defer iter.Close()
	for ; iter.Valid(); iter.Next() {
		v, err := iter.Value()
		if err != nil {
			return err
		}
		if cb(v) {
			return nil
		}
	}
	return nil
}
