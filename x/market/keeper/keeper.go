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

// createMarket persists a brand-new Market record. The ExpiryIndex is
// populated only when the market is "auto-expiry eligible" — ACTIVE
// with a future ExpiryTimestamp. Genesis may legitimately carry a
// recovered EXPIRED market with its original ExpiryTimestamp baked in
// for audit; such markets must NOT re-enter the auto-expiry loop, so
// the indexing guard mirrors updateMarket's want-indexed rule.
//
// Caller MUST have verified no record exists at `m.MarketIndex`;
// Markets.Set unconditionally overwrites and would silently destroy a
// real market. Used by MsgCreateMarket and InitGenesis.
func (k Keeper) createMarket(ctx context.Context, m types.Market) error {
	if err := k.Markets.Set(ctx, m.MarketIndex, m); err != nil {
		return err
	}
	if m.Status == perptypes.MarketStatusActive && m.ExpiryTimestamp > 0 {
		if err := k.ExpiryIndex.Set(ctx, collections.Join(m.ExpiryTimestamp, m.MarketIndex)); err != nil {
			return err
		}
	}
	return nil
}

// updateMarket overwrites an existing Market record and keeps the
// ExpiryIndex in sync. The secondary index only tracks markets that
// are eligible for auto-expiry (i.e. ACTIVE with a future
// ExpiryTimestamp). A change to either `Status` or `ExpiryTimestamp`
// can flip eligibility, so the delta logic is:
//
//   - was-indexed = old was ACTIVE && old.ExpiryTimestamp > 0
//   - want-indexed = new is ACTIVE && new.ExpiryTimestamp > 0
//
// Remove the (old.ExpiryTimestamp, idx) entry whenever it was indexed
// and either the new state is no longer indexed or the timestamp
// changed; symmetrically add the new entry whenever it should be
// indexed and either was not before or the timestamp moved.
//
// Caller MUST pass the in-store record as `old` so the index delta is
// computed against the truth on disk (passing a stale copy can leak
// orphan entries).
func (k Keeper) updateMarket(ctx context.Context, old, m types.Market) error {
	if err := k.Markets.Set(ctx, m.MarketIndex, m); err != nil {
		return err
	}
	wasIndexed := old.Status == perptypes.MarketStatusActive && old.ExpiryTimestamp > 0
	wantsIndexed := m.Status == perptypes.MarketStatusActive && m.ExpiryTimestamp > 0
	expiryChanged := old.ExpiryTimestamp != m.ExpiryTimestamp
	if wasIndexed && (!wantsIndexed || expiryChanged) {
		if err := k.ExpiryIndex.Remove(ctx, collections.Join(old.ExpiryTimestamp, old.MarketIndex)); err != nil {
			return err
		}
	}
	if wantsIndexed && (!wasIndexed || expiryChanged) {
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

// expireMarket flips an ACTIVE market into terminal EXPIRED state.
// It MUST be used by both the EndBlocker auto-expiry path and the
// MsgUpdateMarket(NewStatus=Expired) governance path so the two paths
// produce identical observable effects:
//
//  1. updateMarket writes the new record. The standard delta logic
//     drops the ExpiryIndex entry as a side-effect of the EXPIRED
//     transition (was-indexed → not want-indexed).
//  2. applyMarketExit closes residual positions against the insurance
//     fund via the LiquidationKeeper hook (nil-safe) and emits both
//     EventTypeMarketExpired and, when the exit hook failed,
//     EventTypeMarketExpireExitFailed for monitors.
//
// `m` must be the in-store record (Status=ACTIVE before the call).
func (k Keeper) expireMarket(ctx context.Context, m types.Market) error {
	old := m
	m.Status = perptypes.MarketStatusExpired
	if err := k.updateMarket(ctx, old, m); err != nil {
		return err
	}
	return k.applyMarketExit(ctx, m)
}

// applyMarketExit performs the post-state-write side of an EXPIRED
// transition: invoke the liquidation hook (nil-safe) and emit the
// expired / failure events. Separated from expireMarket so the
// MsgUpdateMarket path, which already routes through updateMarket for
// the field/status changes, can compose the two without a redundant
// re-write.
func (k Keeper) applyMarketExit(ctx context.Context, m types.Market) error {
	sdkCtx := sdk.UnwrapSDKContext(ctx)
	var exitErr error
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

// iterateExpired walks the ExpiryIndex for every (expiry, marketIdx)
// entry whose expiry is <= `now` and invokes `visit` on each ACTIVE
// market, up to `budget` invocations. Stale entries (missing market
// record, or already non-ACTIVE) are silently dropped from the index
// — they're a recovery path for divergence, not the common case. A
// per-market `visit` error is logged but does not abort the loop, so
// one bad market cannot stall EndBlocker.
//
// The iterator is the canonical lever the EndBlocker uses to keep the
// auto-expiry cost O(expired count) rather than O(all markets).
func (k Keeper) iterateExpired(ctx context.Context, now int64, budget uint32, visit func(types.Market) error) error {
	if budget == 0 {
		return nil
	}
	sdkCtx := sdk.UnwrapSDKContext(ctx)
	rng := new(collections.Range[collections.Pair[int64, uint32]]).
		EndInclusive(collections.Join(now, uint32(math.MaxUint32)))
	iter, err := k.ExpiryIndex.Iterate(ctx, rng)
	if err != nil {
		return err
	}
	defer iter.Close()
	processed := uint32(0)
	for ; iter.Valid() && processed < budget; iter.Next() {
		key, err := iter.Key()
		if err != nil {
			sdkCtx.Logger().Error("market: expiry index iter key", "err", err)
			continue
		}
		idx := key.K2()
		m, err := k.GetMarket(ctx, idx)
		if err != nil {
			sdkCtx.Logger().Error("market: expiry index drift, removing entry",
				"market", idx, "err", err)
			_ = k.ExpiryIndex.Remove(ctx, key)
			continue
		}
		if m.Status != perptypes.MarketStatusActive {
			_ = k.ExpiryIndex.Remove(ctx, key)
			continue
		}
		if err := visit(m); err != nil {
			sdkCtx.Logger().Error("market: visit failed",
				"market", idx, "err", err)
			continue
		}
		processed++
	}
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

func (k Keeper) IsPerpsMarket(ctx context.Context, idx uint32) bool {
	p, err := k.Params.Get(ctx)
	if err != nil {
		// Fall back to the chain-wide constants if Params are
		// unavailable (e.g. very early in app boot before
		// InitGenesis).
		return idx <= perptypes.MaxPerpsMarketIndex && idx != perptypes.NilMarketIndex
	}
	return p.IsPerpsIndex(idx)
}

func (k Keeper) IsSpotMarket(ctx context.Context, idx uint32) bool {
	p, err := k.Params.Get(ctx)
	if err != nil {
		return idx >= perptypes.MinSpotMarketIndex && idx <= perptypes.MaxSpotMarketIndex
	}
	return p.IsSpotIndex(idx)
}

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
