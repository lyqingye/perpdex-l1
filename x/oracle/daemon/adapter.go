package daemon

import (
	"context"

	assetkeeper "github.com/perpdex/perpdex-l1/x/asset/keeper"
	marketkeeper "github.com/perpdex/perpdex-l1/x/market/keeper"
	markettypes "github.com/perpdex/perpdex-l1/x/market/types"
	"github.com/perpdex/perpdex-l1/x/oracle/types"
)

// MarketKeeperAdapter wraps an x/market keeper so it satisfies the daemon's
// `MarketReader` interface. The wrapping is necessary because the daemon
// cannot import x/market directly without creating an import cycle through
// x/oracle.
type MarketKeeperAdapter struct {
	K marketkeeper.Keeper
}

// IterateMarkets implements MarketReader by walking the underlying keeper
// and projecting each Market into a oracle types.MarketShim.
func (a MarketKeeperAdapter) IterateMarkets(ctx context.Context, cb func(types.MarketShim) bool) error {
	return a.K.IterateMarkets(ctx, func(m markettypes.Market) bool {
		return cb(types.MarketShim{
			MarketIndex:  m.MarketIndex,
			BaseAssetID:  m.BaseAssetId,
			QuoteAssetID: m.QuoteAssetId,
			Decimals:     defaultPriceDecimals(),
		})
	})
}

// AssetKeeperAdapter wraps an x/asset keeper as `AssetReader`.
type AssetKeeperAdapter struct {
	K assetkeeper.Keeper
}

// GetAssetByIndex returns the (display name, decimals) pair for the asset
// indexed at idx. It is the only operation the resolver performs.
func (a AssetKeeperAdapter) GetAssetByIndex(ctx context.Context, idx uint32) (string, uint32, error) {
	asset, err := a.K.GetAsset(ctx, idx)
	if err != nil {
		return "", 0, err
	}
	return asset.DisplayName, asset.Decimals, nil
}

// defaultPriceDecimals returns the chain-side price precision for a market
// when the on-chain Market record does not carry a market-specific override.
//
// Two decimals (price * 100) is the maximum that fits a $429M asset into a
// uint32 without overflow, which is comfortably above any actively traded
// instrument. Markets with extreme valuations (e.g. shitcoin-vs-USD with a
// price < 0.01) need a separate decimals override; that override is the
// `Decimals` field of `oracle/types.MarketShim` and operators can populate
// it once we add a corresponding field to the on-chain Market message.
func defaultPriceDecimals() uint8 { return 2 }
