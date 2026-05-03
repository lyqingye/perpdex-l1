package types

import (
	"context"

	assettypes "github.com/perpdex/perpdex-l1/x/asset/types"
)

type AssetKeeper interface {
	GetAsset(ctx context.Context, index uint32) (assettypes.Asset, error)
}

// LiquidationKeeper is invoked by x/market EndBlocker when a market expires
// to forcibly close all open positions.
type LiquidationKeeper interface {
	ApplyExitPosition(ctx context.Context, marketIndex uint32) error
}
