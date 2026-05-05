package app

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"

	"github.com/gorilla/mux"

	abci "github.com/cometbft/cometbft/abci/types"
	tmjson "github.com/cometbft/cometbft/libs/json"

	dbm "github.com/cosmos/cosmos-db"
	"github.com/cosmos/gogoproto/proto"
	ibctm "github.com/cosmos/ibc-go/v10/modules/light-clients/07-tendermint"
	ibctesting "github.com/cosmos/ibc-go/v10/testing"

	autocliv1 "cosmossdk.io/api/cosmos/autocli/v1"
	reflectionv1 "cosmossdk.io/api/cosmos/reflection/v1"
	"cosmossdk.io/client/v2/autocli"
	"cosmossdk.io/core/appmodule"
	"cosmossdk.io/log"
	"cosmossdk.io/x/tx/signing"
	upgradetypes "cosmossdk.io/x/upgrade/types"

	"github.com/cosmos/cosmos-sdk/baseapp"
	"github.com/cosmos/cosmos-sdk/client"
	"github.com/cosmos/cosmos-sdk/client/grpc/cmtservice"
	nodeservice "github.com/cosmos/cosmos-sdk/client/grpc/node"
	"github.com/cosmos/cosmos-sdk/codec"
	"github.com/cosmos/cosmos-sdk/codec/address"
	"github.com/cosmos/cosmos-sdk/codec/types"
	"github.com/cosmos/cosmos-sdk/runtime"
	runtimeservices "github.com/cosmos/cosmos-sdk/runtime/services"
	"github.com/cosmos/cosmos-sdk/server"
	"github.com/cosmos/cosmos-sdk/server/api"
	"github.com/cosmos/cosmos-sdk/server/config"
	servertypes "github.com/cosmos/cosmos-sdk/server/types"
	"github.com/cosmos/cosmos-sdk/std"
	"github.com/cosmos/cosmos-sdk/testutil/testdata"
	sdk "github.com/cosmos/cosmos-sdk/types"
	"github.com/cosmos/cosmos-sdk/types/module"
	"github.com/cosmos/cosmos-sdk/types/msgservice"
	sigtypes "github.com/cosmos/cosmos-sdk/types/tx/signing"
	"github.com/cosmos/cosmos-sdk/version"
	"github.com/cosmos/cosmos-sdk/x/auth/ante"
	authcodec "github.com/cosmos/cosmos-sdk/x/auth/codec"
	authtx "github.com/cosmos/cosmos-sdk/x/auth/tx"
	txmodule "github.com/cosmos/cosmos-sdk/x/auth/tx/config"
	authtypes "github.com/cosmos/cosmos-sdk/x/auth/types"
	govkeeper "github.com/cosmos/cosmos-sdk/x/gov/keeper"
	govtypes "github.com/cosmos/cosmos-sdk/x/gov/types"

	perpante "github.com/perpdex/perpdex-l1/ante"
	"github.com/perpdex/perpdex-l1/app/keepers"
	"github.com/perpdex/perpdex-l1/app/upgrades"
	oraclekeeper "github.com/perpdex/perpdex-l1/x/oracle/keeper"
)

var (
	// DefaultNodeHome is the default home for the perpd binary.
	DefaultNodeHome string

	// Upgrades lists every chain upgrade known to this binary.
	Upgrades = []upgrades.Upgrade{}
)

var (
	_ runtime.AppI            = (*PerpDEXApp)(nil)
	_ servertypes.Application = (*PerpDEXApp)(nil)
	_ ibctesting.TestingApp   = (*PerpDEXApp)(nil)
)

// PerpDEXApp is the ABCI application for the PerpDEX L1 chain. It embeds the
// generated AppKeepers struct so that keepers can be accessed directly via
// the app value (matching the Gaia layout).
type PerpDEXApp struct {
	*baseapp.BaseApp
	keepers.AppKeepers

	legacyAmino       *codec.LegacyAmino
	appCodec          codec.Codec
	txConfig          client.TxConfig
	interfaceRegistry types.InterfaceRegistry

	mm           *module.Manager
	ModuleBasics module.BasicManager

	sm           *module.SimulationManager
	configurator module.Configurator
}

func init() {
	userHomeDir, err := os.UserHomeDir()
	if err != nil {
		panic(err)
	}
	DefaultNodeHome = filepath.Join(userHomeDir, ".perpd")
}

// NewPerpDEXApp builds a fully wired PerpDEXApp.
func NewPerpDEXApp(
	logger log.Logger,
	db dbm.DB,
	traceStore io.Writer,
	loadLatest bool,
	skipUpgradeHeights map[int64]bool,
	homePath string,
	appOpts servertypes.AppOptions,
	baseAppOptions ...func(*baseapp.BaseApp),
) *PerpDEXApp {
	legacyAmino := codec.NewLegacyAmino()
	interfaceRegistry, err := types.NewInterfaceRegistryWithOptions(types.InterfaceRegistryOptions{
		ProtoFiles: proto.HybridResolver,
		SigningOptions: signing.Options{
			AddressCodec: address.Bech32Codec{
				Bech32Prefix: sdk.GetConfig().GetBech32AccountAddrPrefix(),
			},
			ValidatorAddressCodec: address.Bech32Codec{
				Bech32Prefix: sdk.GetConfig().GetBech32ValidatorAddrPrefix(),
			},
		},
	})
	if err != nil {
		panic(err)
	}
	appCodec := codec.NewProtoCodec(interfaceRegistry)
	txConfig := authtx.NewTxConfig(appCodec, authtx.DefaultSignModes)

	std.RegisterLegacyAminoCodec(legacyAmino)
	std.RegisterInterfaces(interfaceRegistry)

	bApp := baseapp.NewBaseApp(
		AppName,
		logger,
		db,
		txConfig.TxDecoder(),
		baseAppOptions...,
	)
	bApp.SetCommitMultiStoreTracer(traceStore)
	bApp.SetVersion(version.Version)
	bApp.SetInterfaceRegistry(interfaceRegistry)
	bApp.SetTxEncoder(txConfig.TxEncoder())

	app := &PerpDEXApp{
		BaseApp:           bApp,
		legacyAmino:       legacyAmino,
		txConfig:          txConfig,
		appCodec:          appCodec,
		interfaceRegistry: interfaceRegistry,
	}

	moduleAccountAddresses := app.ModuleAccountAddrs()

	app.AppKeepers = keepers.NewAppKeeper(
		appCodec,
		bApp,
		legacyAmino,
		maccPerms,
		moduleAccountAddresses,
		app.BlockedModuleAccountAddrs(moduleAccountAddresses),
		skipUpgradeHeights,
		homePath,
		logger,
		appOpts,
	)

	// Wire the 07-tendermint light client into the IBC client router.
	clientKeeper := app.IBCKeeper.ClientKeeper
	tmLightClientModule := ibctm.NewLightClientModule(appCodec, clientKeeper.GetStoreProvider())
	clientKeeper.AddRoute(ibctm.ModuleName, &tmLightClientModule)

	app.mm = module.NewManager(appModules(app, appCodec, txConfig, tmLightClientModule)...)
	app.ModuleBasics = newBasicManagerFromManager(app)

	enabledSignModes := append([]sigtypes.SignMode(nil), authtx.DefaultSignModes...)
	enabledSignModes = append(enabledSignModes, sigtypes.SignMode_SIGN_MODE_TEXTUAL)

	txConfigOpts := authtx.ConfigOptions{
		EnabledSignModes:           enabledSignModes,
		TextualCoinMetadataQueryFn: txmodule.NewBankKeeperCoinMetadataQueryFn(app.BankKeeper),
	}
	txConfig, err = authtx.NewTxConfigWithOptions(appCodec, txConfigOpts)
	if err != nil {
		panic(err)
	}
	app.txConfig = txConfig

	// upgrade module is required to run before authentication.
	app.mm.SetOrderPreBlockers(
		upgradetypes.ModuleName,
		authtypes.ModuleName,
	)
	app.mm.SetOrderBeginBlockers(orderBeginBlockers()...)
	app.mm.SetOrderEndBlockers(orderEndBlockers()...)
	app.mm.SetOrderInitGenesis(orderInitBlockers()...)

	app.configurator = module.NewConfigurator(app.appCodec, app.MsgServiceRouter(), app.GRPCQueryRouter())
	if err := app.mm.RegisterServices(app.configurator); err != nil {
		panic(err)
	}

	autocliv1.RegisterQueryServer(app.GRPCQueryRouter(), runtimeservices.NewAutoCLIQueryService(app.mm.Modules))

	reflectionSvc, err := runtimeservices.NewReflectionService()
	if err != nil {
		panic(err)
	}
	reflectionv1.RegisterReflectionServiceServer(app.GRPCQueryRouter(), reflectionSvc)

	testdata.RegisterQueryServer(app.GRPCQueryRouter(), testdata.QueryImpl{})

	app.sm = module.NewSimulationManager(simulationModules(app, appCodec)...)
	app.sm.RegisterStoreDecoders()

	app.MountKVStores(app.GetKVStoreKey())
	app.MountTransientStores(app.GetTransientStoreKey())
	app.MountMemoryStores(app.GetMemoryStoreKey())

	govModuleAddr := authtypes.NewModuleAddress(govtypes.ModuleName).String()
	anteHandler, err := perpante.NewAnteHandler(perpante.HandlerOptions{
		HandlerOptions: ante.HandlerOptions{
			AccountKeeper:   app.AccountKeeper,
			BankKeeper:      app.BankKeeper,
			FeegrantKeeper:  app.FeeGrantKeeper,
			SignModeHandler: txConfig.SignModeHandler(),
			SigGasConsumer:  ante.DefaultSigVerificationGasConsumer,
		},
		IBCKeeper:          app.IBCKeeper,
		OracleGovAuthority: govModuleAddr,
	})
	if err != nil {
		panic(fmt.Errorf("failed to create AnteHandler: %w", err))
	}

	app.SetAnteHandler(anteHandler)
	app.SetInitChainer(app.InitChainer)
	app.SetPreBlocker(app.PreBlocker)
	app.SetBeginBlocker(app.BeginBlocker)
	app.SetEndBlocker(app.EndBlocker)

	// Wire the dydx/Slinky-style ABCI++ vote-extension oracle pipeline.
	// The handlers no-op while consensus has not yet enabled vote
	// extensions (see ConsensusParams.Abci.VoteExtensionsEnableHeight);
	// once active they aggregate the previous block's price votes and
	// inject MsgAggregateOracleVotes as the first transaction of every
	// new block.
	voteExtHandler := oraclekeeper.NewVoteExtensionHandler(app.OracleKeeper, app.txConfig, govModuleAddr)
	bApp.SetExtendVoteHandler(voteExtHandler.ExtendVote())
	bApp.SetVerifyVoteExtensionHandler(voteExtHandler.VerifyVoteExtension())
	defaultProposalHandler := baseapp.NewDefaultProposalHandler(nil, bApp)
	bApp.SetPrepareProposal(voteExtHandler.PrepareProposal(defaultProposalHandler.PrepareProposalHandler()))
	bApp.SetProcessProposal(voteExtHandler.ProcessProposal(defaultProposalHandler.ProcessProposalHandler()))

	app.setupUpgradeHandlers()
	app.setupUpgradeStoreLoaders()

	protoFiles, err := proto.MergedRegistry()
	if err != nil {
		panic(err)
	}
	if err := msgservice.ValidateProtoAnnotations(protoFiles); err != nil {
		// Mirrors the Gaia behavior: log but do not panic so that future
		// SDK upgrades that introduce new annotations are not breaking.
		fmt.Fprintln(os.Stderr, err.Error())
	}

	if loadLatest {
		if err := app.LoadLatestVersion(); err != nil {
			panic(fmt.Sprintf("failed to load latest version: %s", err))
		}
	}

	return app
}

// Name returns the application name.
func (app *PerpDEXApp) Name() string { return app.BaseApp.Name() }

// PreBlocker is invoked before every block; it runs the upgrade module first.
func (app *PerpDEXApp) PreBlocker(ctx sdk.Context, _ *abci.RequestFinalizeBlock) (*sdk.ResponsePreBlock, error) {
	return app.mm.PreBlock(ctx)
}

func (app *PerpDEXApp) BeginBlocker(ctx sdk.Context) (sdk.BeginBlock, error) {
	return app.mm.BeginBlock(ctx)
}

func (app *PerpDEXApp) EndBlocker(ctx sdk.Context) (sdk.EndBlock, error) {
	return app.mm.EndBlock(ctx)
}

// DefaultVoteExtensionsEnableHeight is the height at which the chain
// flips on ABCI++ vote extensions when the genesis file does not pin a
// custom value. Setting it to 2 mirrors the dydx default (the very first
// block after genesis emits VEs because cometbft can only enable VEs
// starting from height >= H+1 after the consensus param is set in InitChain).
const DefaultVoteExtensionsEnableHeight = int64(2)

// InitChainer is the entrypoint invoked by Tendermint at genesis time.
func (app *PerpDEXApp) InitChainer(ctx sdk.Context, req *abci.RequestInitChain) (*abci.ResponseInitChain, error) {
	var genesisState GenesisState
	if err := tmjson.Unmarshal(req.AppStateBytes, &genesisState); err != nil {
		panic(err)
	}
	if err := app.UpgradeKeeper.SetModuleVersionMap(ctx, app.mm.GetVersionMap()); err != nil {
		panic(err)
	}

	resp, err := app.mm.InitGenesis(ctx, app.appCodec, genesisState)
	if err != nil {
		panic(err)
	}
	// Force-enable ABCI++ vote extensions on genesis if the operator did
	// not pin a value. Without this the oracle VE pipeline would never
	// activate, leaving the chain without on-chain price updates.
	if resp.ConsensusParams != nil && resp.ConsensusParams.Abci != nil &&
		resp.ConsensusParams.Abci.VoteExtensionsEnableHeight == 0 {
		resp.ConsensusParams.Abci.VoteExtensionsEnableHeight = DefaultVoteExtensionsEnableHeight
	}
	return resp, nil
}

// LoadHeight loads a particular committed height from disk.
func (app *PerpDEXApp) LoadHeight(height int64) error {
	return app.LoadVersion(height)
}

// ModuleAccountAddrs returns all module account addresses managed by the app.
func (app *PerpDEXApp) ModuleAccountAddrs() map[string]bool {
	modAccAddrs := make(map[string]bool)
	for acc := range maccPerms {
		modAccAddrs[authtypes.NewModuleAddress(acc).String()] = true
	}
	return modAccAddrs
}

// BlockedModuleAccountAddrs returns module account addresses that may NOT
// receive funds. The gov module is intentionally allowed so that it can hold
// proposal deposits.
func (app *PerpDEXApp) BlockedModuleAccountAddrs(modAccAddrs map[string]bool) map[string]bool {
	delete(modAccAddrs, authtypes.NewModuleAddress(govtypes.ModuleName).String())
	return modAccAddrs
}

func (app *PerpDEXApp) LegacyAmino() *codec.LegacyAmino {
	return app.legacyAmino
}

func (app *PerpDEXApp) AppCodec() codec.Codec {
	return app.appCodec
}

func (app *PerpDEXApp) DefaultGenesis() map[string]json.RawMessage {
	return app.ModuleBasics.DefaultGenesis(app.appCodec)
}

func (app *PerpDEXApp) InterfaceRegistry() types.InterfaceRegistry {
	return app.interfaceRegistry
}

func (app *PerpDEXApp) SimulationManager() *module.SimulationManager {
	return app.sm
}

// RegisterAPIRoutes registers all module REST routes against the API server.
func (app *PerpDEXApp) RegisterAPIRoutes(apiSvr *api.Server, apiConfig config.APIConfig) {
	clientCtx := apiSvr.ClientCtx
	authtx.RegisterGRPCGatewayRoutes(clientCtx, apiSvr.GRPCGatewayRouter)
	cmtservice.RegisterGRPCGatewayRoutes(clientCtx, apiSvr.GRPCGatewayRouter)
	app.ModuleBasics.RegisterGRPCGatewayRoutes(clientCtx, apiSvr.GRPCGatewayRouter)
	nodeservice.RegisterGRPCGatewayRoutes(clientCtx, apiSvr.GRPCGatewayRouter)

	if err := server.RegisterSwaggerAPI(apiSvr.ClientCtx, apiSvr.Router, apiConfig.Swagger); err != nil {
		panic(err)
	}
}

func (app *PerpDEXApp) RegisterNodeService(clientCtx client.Context, cfg config.Config) {
	nodeservice.RegisterNodeService(clientCtx, app.GRPCQueryRouter(), cfg)
}

func (app *PerpDEXApp) RegisterTxService(clientCtx client.Context) {
	authtx.RegisterTxService(app.GRPCQueryRouter(), clientCtx, app.Simulate, app.interfaceRegistry)
}

func (app *PerpDEXApp) RegisterTendermintService(clientCtx client.Context) {
	cmtservice.RegisterTendermintService(
		clientCtx,
		app.GRPCQueryRouter(),
		app.interfaceRegistry,
		app.Query,
	)
}

// setupUpgradeStoreLoaders applies any pending store upgrades to the
// underlying multistore so that newly-added keepers find their stores.
func (app *PerpDEXApp) setupUpgradeStoreLoaders() {
	upgradeInfo, err := app.UpgradeKeeper.ReadUpgradeInfoFromDisk()
	if err != nil {
		panic(fmt.Sprintf("failed to read upgrade info from disk %s", err))
	}
	if app.UpgradeKeeper.IsSkipHeight(upgradeInfo.Height) {
		return
	}
	for _, upgrade := range Upgrades {
		upgrade := upgrade
		if upgradeInfo.Name == upgrade.UpgradeName {
			storeUpgrades := upgrade.StoreUpgrades
			app.SetStoreLoader(upgradetypes.UpgradeStoreLoader(upgradeInfo.Height, &storeUpgrades))
		}
	}
}

func (app *PerpDEXApp) setupUpgradeHandlers() {
	for _, upgrade := range Upgrades {
		app.UpgradeKeeper.SetUpgradeHandler(
			upgrade.UpgradeName,
			upgrade.CreateUpgradeHandler(app.mm, app.configurator, &app.AppKeepers),
		)
	}
}

// RegisterSwaggerAPI mounts the bundled Swagger UI under /swagger/ on the
// given mux router. It is a no-op if no swagger filesystem is embedded.
func RegisterSwaggerAPI(rtr *mux.Router) {
	rtr.PathPrefix("/swagger/").Handler(http.NotFoundHandler())
}

// AutoCliOpts builds the autocli options used by the CLI to expose every
// module's RPC commands.
func (app *PerpDEXApp) AutoCliOpts() autocli.AppOptions {
	modules := make(map[string]appmodule.AppModule)
	for _, m := range app.mm.Modules {
		if moduleWithName, ok := m.(module.HasName); ok {
			moduleName := moduleWithName.Name()
			if appModule, ok := moduleWithName.(appmodule.AppModule); ok {
				modules[moduleName] = appModule
			}
		}
	}

	return autocli.AppOptions{
		Modules:               modules,
		AddressCodec:          authcodec.NewBech32Codec(sdk.GetConfig().GetBech32AccountAddrPrefix()),
		ValidatorAddressCodec: authcodec.NewBech32Codec(sdk.GetConfig().GetBech32ValidatorAddrPrefix()),
		ConsensusAddressCodec: authcodec.NewBech32Codec(sdk.GetConfig().GetBech32ConsensusAddrPrefix()),
	}
}

// TestingApp interface implementations for ibc-go test helpers.

func (app *PerpDEXApp) GetBaseApp() *baseapp.BaseApp { return app.BaseApp }

func (app *PerpDEXApp) GetTxConfig() client.TxConfig { return app.txConfig }

func (app *PerpDEXApp) GetTestGovKeeper() *govkeeper.Keeper { return app.GovKeeper }

// EmptyAppOptions stub for tests.
type EmptyAppOptions struct{}

func (EmptyAppOptions) Get(_ string) interface{} { return nil }
