package types

import (
	"context"

	"cosmossdk.io/math"

	accounttypes "github.com/perpdex/perpdex-l1/x/account/types"
	markettypes "github.com/perpdex/perpdex-l1/x/market/types"
	orderbooktypes "github.com/perpdex/perpdex-l1/x/orderbook/types"
	tradekeeper "github.com/perpdex/perpdex-l1/x/trade/keeper"
)

type AccountKeeper interface {
	GetAccount(ctx context.Context, idx uint64) (accounttypes.Account, error)
	GetPosition(ctx context.Context, accIdx uint64, marketIdx uint32) (accounttypes.AccountPosition, error)
	IsAuthorized(ctx context.Context, signer string, idx uint64) (bool, error)
	// AvailableBalance is used by the spot CreateOrder pre-check so a
	// taker that under-funds its residue is force-cancelled without
	// reverting the whole Msg (preserving any prior fills).
	AvailableBalance(ctx context.Context, accIdx uint64, assetIdx uint32) (math.Int, error)
}

type MarketKeeper interface {
	GetMarket(ctx context.Context, idx uint32) (markettypes.Market, error)
	GetMarketDetails(ctx context.Context, idx uint32) (markettypes.MarketDetails, error)
	AllocateNonce(ctx context.Context, marketIdx uint32, isAsk bool) (int64, error)
	// GetMarkPriceAndDetails returns the gated (zero + staleness) mark and
	// MarketDetails. Used by the trigger-activation EndBlocker so a
	// stop-loss / take-profit cannot fire on a stale or missing mark.
	GetMarkPriceAndDetails(ctx context.Context, marketIdx uint32) (uint32, markettypes.MarketDetails, error)
}

// OrderbookKeeper is the surface x/matching uses to read and mutate
// orderbook state. The interface is intentionally narrow:
//
//   - read / lookup methods for matching pre-checks and decision logic
//   - iteration helpers for cancel-all and trigger-scan loops
//   - high-level lifecycle methods (Open/Cancel/Fill/Evict/Activate) that
//     own the entry + index + Order-record consistency invariants
//
// All low-level entry / index / trigger primitives now live behind these
// lifecycle methods inside the orderbook keeper, so a matching code path
// can no longer accidentally update the orderbook entry without also
// keeping the Order record and the cancel-all index in sync.
type OrderbookKeeper interface {
	// Read / lookup.
	GetOrder(ctx context.Context, idx uint64) (orderbooktypes.Order, error)
	GetOrderByClientID(ctx context.Context, market uint32, account uint64, clientID uint64) (orderbooktypes.Order, error)
	// HasOpenClientOrder reports whether (market, account, clientID)
	// currently resolves to an open/pending order in the book.
	HasOpenClientOrder(ctx context.Context, market uint32, account uint64, clientID uint64) (bool, uint64, error)
	PeekBestOpposite(ctx context.Context, market uint32, isAsk bool) (orderbooktypes.OrderBookEntry, bool, error)
	WouldCross(ctx context.Context, market uint32, isAsk bool, price uint32) (bool, error)
	AllocateOrderIndex(ctx context.Context) (uint64, error)

	// Iteration.
	IterateTriggers(ctx context.Context, cb func(market uint32, triggerPrice uint32, orderIndex uint64) bool) error
	// IterateAccountOpenOrders yields every resting order owned by
	// `account`, optionally filtered by market (0 = all markets).
	IterateAccountOpenOrders(ctx context.Context, account uint64, marketFilter uint32, cb func(orderbooktypes.Order) bool) error

	// Lifecycle. Each of these atomically maintains the orderbook entry,
	// the Order record, and the (client + account-open + trigger)
	// indexes that go with it.

	// OpenOrder accepts a freshly-created or post-match Order. When the
	// status is Open / PartiallyFilled the order is rested on the book
	// and indexed; terminal statuses (Filled / Cancelled) are persisted
	// without an entry so IOC residue and zero-fill orders cannot leak.
	OpenOrder(ctx context.Context, o orderbooktypes.Order, isPostOnly bool) error
	// OpenTriggerOrder parks a stop/take order in the trigger index
	// while still indexing it for cancel-all reach.
	OpenTriggerOrder(ctx context.Context, o orderbooktypes.Order) error
	// ActivateTrigger removes a trigger-pending order from the trigger
	// index and flips its status back to Open. The caller mutates the
	// activated variant (LimitOrder / MarketOrder) and runs match next.
	ActivateTrigger(ctx context.Context, orderIndex uint64) (orderbooktypes.Order, error)
	// FillMakerOrder applies `filledBase` against a resting maker,
	// updating both the orderbook entry and the Order record, and
	// clearing client + account-open indexes on full fill.
	FillMakerOrder(ctx context.Context, makerIndex uint64, filledBase uint64) (orderbooktypes.Order, error)
	// EvictMakerOrder removes a maker mid-match (GTT expired or
	// reduce-only invalid) and marks the underlying Order with the
	// supplied terminal status, cleaning up indexes.
	EvictMakerOrder(ctx context.Context, makerIndex uint64, terminalStatus uint32) (orderbooktypes.Order, error)
	// CancelOrder is the unified cancel entrypoint for user cancels,
	// liquidation cancel-all, and GTT-expiry sweeps. It branches on
	// status to remove either the orderbook entry or the trigger
	// registration, sets status to Cancelled, and clears indexes.
	CancelOrder(ctx context.Context, orderIndex uint64) (orderbooktypes.Order, error)

	// GetAccountOpenOrderCount returns the current number of open
	// (resting + trigger-pending) orders held by `account` in
	// `market`. Used by CreateOrder to enforce the per-market cap
	// from Market.MaxOpenOrdersPerAccount.
	GetAccountOpenOrderCount(ctx context.Context, accIdx uint64, marketIdx uint32) (uint32, error)
}

type TradeKeeper interface {
	ApplyPerpsMatching(ctx context.Context, f tradekeeper.PerpFill) error
	ApplySpotMatching(ctx context.Context, f tradekeeper.SpotFill, baseAssetID, quoteAssetID uint32) error
}

// RiskKeeper exposes the post-state health classification used by the
// pre-liquidation order placement gate: accounts in PRE may only
// submit orders that strictly reduce exposure (reduce-only); accounts
// in PARTIAL/FULL/BANKRUPTCY may not submit any user-initiated order
// until liquidation completes.
type RiskKeeper interface {
	GetHealthStatus(ctx context.Context, accountIdx uint64) (uint32, error)
	GetIsolatedHealthStatus(ctx context.Context, accountIdx uint64, marketIdx uint32) (uint32, error)
}
