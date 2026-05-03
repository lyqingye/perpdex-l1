package cmd

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strconv"

	"cosmossdk.io/math"

	"github.com/spf13/cobra"

	"github.com/cosmos/cosmos-sdk/client"
	"github.com/cosmos/cosmos-sdk/client/flags"
	"github.com/cosmos/cosmos-sdk/server"
	genutiltypes "github.com/cosmos/cosmos-sdk/x/genutil/types"

	perptypes "github.com/perpdex/perpdex-l1/types"
	assettypes "github.com/perpdex/perpdex-l1/x/asset/types"
	markettypes "github.com/perpdex/perpdex-l1/x/market/types"
)

// addGenesisAssetCmd inserts an Asset entry into the asset module's
// genesis state. Useful for chain operators that want to register additional
// collateral or spot assets at genesis time without crafting JSON by hand.
func addGenesisAssetCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "add-genesis-asset [asset-index] [denom] [display-name] [decimals] [extension-multiplier] [margin-mode]",
		Short: "Add an asset entry to genesis.json",
		Long: `Append an Asset record to the x/asset module genesis. The command edits the
genesis.json located under the configured home directory in-place.`,
		Args: cobra.ExactArgs(6),
		RunE: func(cmd *cobra.Command, args []string) error {
			clientCtx := client.GetClientContextFromCmd(cmd)
			cdc := clientCtx.Codec
			serverCtx := server.GetServerContextFromCmd(cmd)
			cfg := serverCtx.Config
			cfg.SetRoot(clientCtx.HomeDir)

			genFile := cfg.GenesisFile()
			appGenesis, appState, err := loadAppGenesis(genFile)
			if err != nil {
				return err
			}

			assetIndex, err := strconv.ParseUint(args[0], 10, 32)
			if err != nil {
				return fmt.Errorf("invalid asset_index: %w", err)
			}
			decimals, err := strconv.ParseUint(args[3], 10, 32)
			if err != nil {
				return fmt.Errorf("invalid decimals: %w", err)
			}
			extensionMultiplier, err := strconv.ParseUint(args[4], 10, 64)
			if err != nil {
				return fmt.Errorf("invalid extension_multiplier: %w", err)
			}
			marginMode, err := strconv.ParseUint(args[5], 10, 32)
			if err != nil {
				return fmt.Errorf("invalid margin_mode: %w", err)
			}

			var assetGS assettypes.GenesisState
			rawAsset := appState[assettypes.ModuleName]
			if len(rawAsset) > 0 {
				if err := cdc.UnmarshalJSON(rawAsset, &assetGS); err != nil {
					return fmt.Errorf("failed to unmarshal asset genesis: %w", err)
				}
			} else {
				assetGS = *assettypes.DefaultGenesis()
			}

			for _, a := range assetGS.Assets {
				if a.AssetIndex == uint32(assetIndex) {
					return fmt.Errorf("asset_index %d already exists in genesis", assetIndex)
				}
				if a.Denom == args[1] {
					return fmt.Errorf("denom %q already exists in genesis", args[1])
				}
			}

			assetGS.Assets = append(assetGS.Assets, assettypes.Asset{
				AssetIndex:          uint32(assetIndex),
				Denom:               args[1],
				DisplayName:         args[2],
				Decimals:            uint32(decimals),
				ExtensionMultiplier: extensionMultiplier,
				MinTransferAmount:   perptypes.MinPartialTransferAmount,
				MinWithdrawalAmount: perptypes.MinPartialWithdrawAmount,
				MarginMode:          uint32(marginMode),
				Enabled:             true,
				CreatedAt:           0,
			})
			if uint32(assetIndex) >= assetGS.NextAssetIndex {
				assetGS.NextAssetIndex = uint32(assetIndex) + 1
			}
			if err := assetGS.Validate(); err != nil {
				return err
			}
			rawAsset, err = cdc.MarshalJSON(&assetGS)
			if err != nil {
				return err
			}
			appState[assettypes.ModuleName] = rawAsset

			return saveAppGenesis(genFile, appGenesis, appState)
		},
	}
	cmd.Flags().String(flags.FlagHome, "", "The application home directory")
	return cmd
}

// addGenesisMarketCmd registers a perp/spot Market (with its MarketDetails)
// into the x/market genesis state. Margin parameters are quoted in basis-point
// margin-tick units (see types/constants.go MarginTick).
func addGenesisMarketCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "add-genesis-market [market-index] [market-type] [base-asset] [quote-asset] [taker-fee] [maker-fee] [imf] [mmf] [oi-limit]",
		Short: "Add a Market entry to genesis.json",
		Args:  cobra.ExactArgs(9),
		RunE: func(cmd *cobra.Command, args []string) error {
			clientCtx := client.GetClientContextFromCmd(cmd)
			cdc := clientCtx.Codec
			serverCtx := server.GetServerContextFromCmd(cmd)
			cfg := serverCtx.Config
			cfg.SetRoot(clientCtx.HomeDir)

			genFile := cfg.GenesisFile()
			appGenesis, appState, err := loadAppGenesis(genFile)
			if err != nil {
				return err
			}

			parseU32 := func(s string) (uint32, error) {
				v, err := strconv.ParseUint(s, 10, 32)
				return uint32(v), err
			}
			parseU64 := func(s string) (uint64, error) {
				return strconv.ParseUint(s, 10, 64)
			}

			marketIndex, err := parseU32(args[0])
			if err != nil {
				return fmt.Errorf("invalid market_index: %w", err)
			}
			marketType, err := parseU32(args[1])
			if err != nil {
				return fmt.Errorf("invalid market_type: %w", err)
			}
			baseAsset, err := parseU32(args[2])
			if err != nil {
				return fmt.Errorf("invalid base_asset: %w", err)
			}
			quoteAsset, err := parseU32(args[3])
			if err != nil {
				return fmt.Errorf("invalid quote_asset: %w", err)
			}
			takerFee, err := parseU32(args[4])
			if err != nil {
				return fmt.Errorf("invalid taker_fee: %w", err)
			}
			makerFee, err := parseU32(args[5])
			if err != nil {
				return fmt.Errorf("invalid maker_fee: %w", err)
			}
			imf, err := parseU32(args[6])
			if err != nil {
				return fmt.Errorf("invalid imf: %w", err)
			}
			mmf, err := parseU32(args[7])
			if err != nil {
				return fmt.Errorf("invalid mmf: %w", err)
			}
			oiLimit, err := parseU64(args[8])
			if err != nil {
				return fmt.Errorf("invalid oi_limit: %w", err)
			}

			var marketGS markettypes.GenesisState
			rawMarket := appState[markettypes.ModuleName]
			if len(rawMarket) > 0 {
				if err := cdc.UnmarshalJSON(rawMarket, &marketGS); err != nil {
					return fmt.Errorf("failed to unmarshal market genesis: %w", err)
				}
			} else {
				marketGS = *markettypes.DefaultGenesis()
			}

			for _, m := range marketGS.Markets {
				if m.MarketIndex == marketIndex {
					return fmt.Errorf("market_index %d already exists in genesis", marketIndex)
				}
			}

			marketGS.Markets = append(marketGS.Markets, markettypes.Market{
				MarketIndex:              marketIndex,
				Status:                   perptypes.MarketStatusActive,
				MarketType:               marketType,
				BaseAssetId:              baseAsset,
				QuoteAssetId:             quoteAsset,
				TakerFee:                 takerFee,
				MakerFee:                 makerFee,
				LiquidationFee:           takerFee,
				SizeExtensionMultiplier:  1,
				QuoteExtensionMultiplier: 1,
				MinBaseAmount:            1,
				MinQuoteAmount:           1,
				OrderQuoteLimit:          int64(perptypes.MaxOrderQuoteAmount),
				ExpiryTimestamp:          0,
				CreatedAt:                0,
			})
			marketGS.MarketDetails = append(marketGS.MarketDetails, markettypes.MarketDetails{
				MarketIndex:                  marketIndex,
				DefaultInitialMarginFraction: imf,
				MinInitialMarginFraction:     imf,
				MaintenanceMarginFraction:    mmf,
				CloseOutMarginFraction:       mmf,
				QuoteMultiplier:              1,
				InterestRate:                 0,
				FundingRatePrefixSum:         math.ZeroInt(),
				OpenInterestLimit:            oiLimit,
				AskNonce:                     perptypes.FirstAskNonce,
				BidNonce:                     perptypes.FirstBidNonce,
				FundingClampSmall:            uint32(perptypes.FundingSmallClamp),
				FundingClampBig:              uint32(perptypes.FundingBigClamp),
			})

			if err := marketGS.Validate(); err != nil {
				return err
			}
			rawMarket, err = cdc.MarshalJSON(&marketGS)
			if err != nil {
				return err
			}
			appState[markettypes.ModuleName] = rawMarket

			return saveAppGenesis(genFile, appGenesis, appState)
		},
	}
	cmd.Flags().String(flags.FlagHome, "", "The application home directory")
	return cmd
}

// loadAppGenesis reads and decodes the AppGenesis at the given path returning
// both the wrapper and the parsed application state map (keyed by module name).
func loadAppGenesis(genFile string) (*genutiltypes.AppGenesis, map[string]json.RawMessage, error) {
	if _, err := os.Stat(genFile); err != nil {
		return nil, nil, fmt.Errorf("failed to find genesis file at %s: %w", genFile, err)
	}
	appGenesis, err := genutiltypes.AppGenesisFromFile(genFile)
	if err != nil {
		return nil, nil, fmt.Errorf("failed to read genesis: %w", err)
	}
	var appState map[string]json.RawMessage
	if err := json.Unmarshal(appGenesis.AppState, &appState); err != nil {
		return nil, nil, fmt.Errorf("failed to unmarshal app_state: %w", err)
	}
	return appGenesis, appState, nil
}

// saveAppGenesis re-encodes the given app_state map into the AppGenesis and
// writes the result back to disk, replacing the original genesis.json.
func saveAppGenesis(genFile string, appGenesis *genutiltypes.AppGenesis, appState map[string]json.RawMessage) error {
	raw, err := json.MarshalIndent(appState, "", "  ")
	if err != nil {
		return err
	}
	appGenesis.AppState = raw
	if err := os.MkdirAll(filepath.Dir(genFile), 0o755); err != nil {
		return err
	}
	return appGenesis.SaveAs(genFile)
}
