// Package query offers thin read-only helpers around the perpdex keepers.
//
// They exist mainly to keep test assertions short and to centralise the
// "panic if the chain is in an unexpected state" idiom: each helper either
// returns the value or an error so the caller can decide whether the absence
// of state is a bug or expected.
package query

import (
	"context"
	"errors"

	"cosmossdk.io/collections"
	"cosmossdk.io/math"

	sdk "github.com/cosmos/cosmos-sdk/types"

	perp "github.com/perpdex/perpdex-l1/app"
	accounttypes "github.com/perpdex/perpdex-l1/x/account/types"
	assettypes "github.com/perpdex/perpdex-l1/x/asset/types"
	liquidationtypes "github.com/perpdex/perpdex-l1/x/liquidation/types"
	markettypes "github.com/perpdex/perpdex-l1/x/market/types"
	oracletypes "github.com/perpdex/perpdex-l1/x/oracle/types"
	orderbooktypes "github.com/perpdex/perpdex-l1/x/orderbook/types"
)

// Asset returns the registered Asset record for a given index.
func Asset(app *perp.PerpDEXApp, ctx sdk.Context, index uint32) (assettypes.Asset, error) {
	return app.AssetKeeper.GetAsset(ctx, index)
}

// AssetByDenom looks up an asset by its bank denom.
func AssetByDenom(app *perp.PerpDEXApp, ctx sdk.Context, denom string) (assettypes.Asset, error) {
	return app.AssetKeeper.GetAssetByDenom(ctx, denom)
}

// Account returns the perpdex Account record (master or sub).
func Account(app *perp.PerpDEXApp, ctx sdk.Context, idx uint64) (accounttypes.Account, error) {
	return app.PerpAccountKeeper.GetAccount(ctx, idx)
}

// Collateral returns the (signed) collateral of an account in internal
// precision. Returns ZeroInt if the account collateral has never been set.
func Collateral(app *perp.PerpDEXApp, ctx sdk.Context, idx uint64) (math.Int, error) {
	a, err := app.PerpAccountKeeper.GetAccount(ctx, idx)
	if err != nil {
		return math.ZeroInt(), err
	}
	return a.Collateral, nil
}

// Position returns the (account, market) perp position. Reports zero if
// the position has never been touched (matching keeper's lazy-init).
func Position(
	app *perp.PerpDEXApp,
	ctx sdk.Context,
	accountIdx uint64,
	marketIdx uint32,
) (accounttypes.AccountPosition, error) {
	return app.PerpAccountKeeper.GetPosition(ctx, accountIdx, marketIdx)
}

// PositionSize returns the signed integer position (positive=long).
func PositionSize(
	app *perp.PerpDEXApp,
	ctx sdk.Context,
	accountIdx uint64,
	marketIdx uint32,
) (math.Int, error) {
	p, err := Position(app, ctx, accountIdx, marketIdx)
	if err != nil {
		return math.ZeroInt(), err
	}
	return p.Size_, nil
}

// Market returns the Market struct for a given index.
func Market(app *perp.PerpDEXApp, ctx sdk.Context, idx uint32) (markettypes.Market, error) {
	return app.MarketKeeper.GetMarket(ctx, idx)
}

// MarketDetails returns the MarketDetails struct (funding, IMF chain, etc.).
func MarketDetails(
	app *perp.PerpDEXApp,
	ctx sdk.Context,
	idx uint32,
) (markettypes.MarketDetails, error) {
	return app.MarketKeeper.GetMarketDetails(ctx, idx)
}

// OracleParams returns the current x/oracle params (mode, max_age, etc.).
func OracleParams(app *perp.PerpDEXApp, ctx sdk.Context) (oracletypes.Params, error) {
	return app.OracleKeeper.Params.Get(ctx)
}

// OraclePrice returns the latest aggregated oracle price for a market.
func OraclePrice(
	app *perp.PerpDEXApp,
	ctx sdk.Context,
	marketIdx uint32,
) (oracletypes.OraclePrice, error) {
	return app.OracleKeeper.GetPrice(ctx, marketIdx)
}

// BestBidAsk returns the (bid, ask) prices currently resting at the top of
// the book. Either side may be zero when empty.
func BestBidAsk(
	app *perp.PerpDEXApp,
	ctx sdk.Context,
	marketIdx uint32,
) (uint32, uint32, error) {
	return app.OrderbookKeeper.BestBidAsk(ctx, marketIdx)
}

// Order returns a resting/finished order by its keeper-allocated index.
func Order(
	app *perp.PerpDEXApp,
	ctx sdk.Context,
	orderIdx uint64,
) (orderbooktypes.Order, error) {
	return app.OrderbookKeeper.GetOrder(ctx, orderIdx)
}

// LiquidationFlag fetches the (account, market) liquidation flag if any
// has been raised by the EndBlocker. The boolean discriminates between
// "not flagged" (ok=false) and "fetch error" (err != nil).
func LiquidationFlag(
	app *perp.PerpDEXApp,
	ctx sdk.Context,
	accountIdx uint64,
	marketIdx uint32,
) (liquidationtypes.LiquidationFlag, bool, error) {
	flag, err := app.LiquidationKeeper.Flags.Get(
		context.Context(ctx), collections.Join(accountIdx, marketIdx),
	)
	if errors.Is(err, collections.ErrNotFound) {
		return liquidationtypes.LiquidationFlag{}, false, nil
	}
	if err != nil {
		return liquidationtypes.LiquidationFlag{}, false, err
	}
	return flag, true, nil
}

// HealthStatus runs the risk classifier and returns one of the 5
// HealthHealthy / HealthPreLiquidation / HealthPartialLiquidation /
// HealthFullLiquidation / HealthBankruptcy values.
func HealthStatus(
	app *perp.PerpDEXApp,
	ctx sdk.Context,
	accountIdx uint64,
) (uint32, error) {
	return app.RiskKeeper.GetHealthStatus(ctx, accountIdx)
}

// PublicPoolInfo returns the pool info on a sub-account; second return
// is false when the account is not a public pool.
func PublicPoolInfo(app *perp.PerpDEXApp, ctx sdk.Context, idx uint64) (accounttypes.PublicPoolInfo, bool, error) {
	a, err := app.PerpAccountKeeper.GetAccount(ctx, idx)
	if err != nil {
		return accounttypes.PublicPoolInfo{}, false, err
	}
	if a.PublicPoolInfo == nil {
		return accounttypes.PublicPoolInfo{}, false, nil
	}
	return *a.PublicPoolInfo, true, nil
}

// PublicPoolShares returns the LP entries on a master account.
func PublicPoolShares(app *perp.PerpDEXApp, ctx sdk.Context, idx uint64) ([]accounttypes.PublicPoolShare, error) {
	a, err := app.PerpAccountKeeper.GetAccount(ctx, idx)
	if err != nil {
		return nil, err
	}
	return a.PublicPoolShares, nil
}

// SharesToUSDCValue runs the keeper helper used by burn pricing.
func SharesToUSDCValue(
	app *perp.PerpDEXApp, ctx sdk.Context, poolIdx uint64, shareAmount math.Int,
) (math.Int, error) {
	return app.PerpAccountKeeper.SharesToUSDCValue(ctx, poolIdx, shareAmount)
}

// ADLQueue returns the profit-ranked counterparty queue for a given
// (market, opposite-side). `limit=0` defers to the chain's
// `MaxAdlCandidatesPerVictim` parameter.
func ADLQueue(
	app *perp.PerpDEXApp,
	ctx sdk.Context,
	marketIdx uint32,
	oppositeIsLong bool,
	limit uint32,
) ([]liquidationtypes.ADLCandidate, error) {
	q := app.LiquidationKeeper
	resolvedLimit := limit
	if resolvedLimit == 0 {
		params, err := q.Params.Get(ctx)
		if err != nil {
			return nil, err
		}
		resolvedLimit = params.MaxAdlCandidatesPerVictim
		if resolvedLimit == 0 {
			resolvedLimit = liquidationtypes.DefaultMaxADLCandidatesPerVictim
		}
	}
	cands, err := q.BuildADLQueue(ctx, marketIdx, oppositeIsLong, resolvedLimit)
	if err != nil {
		return nil, err
	}
	out := make([]liquidationtypes.ADLCandidate, 0, len(cands))
	for _, c := range cands {
		out = append(out, liquidationtypes.ADLCandidate{
			AccountIndex:  c.AccountIndex,
			PositionSize:  c.PositionSize,
			UnrealizedPnl: c.UnrealizedPnL,
			Score:         c.Score,
		})
	}
	return out, nil
}
