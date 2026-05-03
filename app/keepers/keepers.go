package keepers

import (
	"os"

	"github.com/cosmos/ibc-go/v10/modules/apps/transfer"
	ibctransferkeeper "github.com/cosmos/ibc-go/v10/modules/apps/transfer/keeper"
	ibctransfertypes "github.com/cosmos/ibc-go/v10/modules/apps/transfer/types"
	ibcclienttypes "github.com/cosmos/ibc-go/v10/modules/core/02-client/types"
	ibcconnectiontypes "github.com/cosmos/ibc-go/v10/modules/core/03-connection/types"
	porttypes "github.com/cosmos/ibc-go/v10/modules/core/05-port/types"
	ibcexported "github.com/cosmos/ibc-go/v10/modules/core/exported"
	ibckeeper "github.com/cosmos/ibc-go/v10/modules/core/keeper"

	"cosmossdk.io/log"
	storetypes "cosmossdk.io/store/types"
	evidencekeeper "cosmossdk.io/x/evidence/keeper"
	evidencetypes "cosmossdk.io/x/evidence/types"
	"cosmossdk.io/x/feegrant"
	feegrantkeeper "cosmossdk.io/x/feegrant/keeper"
	upgradekeeper "cosmossdk.io/x/upgrade/keeper"
	upgradetypes "cosmossdk.io/x/upgrade/types"

	"github.com/cosmos/cosmos-sdk/baseapp"
	"github.com/cosmos/cosmos-sdk/codec"
	"github.com/cosmos/cosmos-sdk/codec/address"
	"github.com/cosmos/cosmos-sdk/runtime"
	servertypes "github.com/cosmos/cosmos-sdk/server/types"
	sdk "github.com/cosmos/cosmos-sdk/types"
	authcodec "github.com/cosmos/cosmos-sdk/x/auth/codec"
	authkeeper "github.com/cosmos/cosmos-sdk/x/auth/keeper"
	authtypes "github.com/cosmos/cosmos-sdk/x/auth/types"
	authzkeeper "github.com/cosmos/cosmos-sdk/x/authz/keeper"
	bankkeeper "github.com/cosmos/cosmos-sdk/x/bank/keeper"
	banktypes "github.com/cosmos/cosmos-sdk/x/bank/types"
	consensusparamkeeper "github.com/cosmos/cosmos-sdk/x/consensus/keeper"
	consensusparamtypes "github.com/cosmos/cosmos-sdk/x/consensus/types"
	distrkeeper "github.com/cosmos/cosmos-sdk/x/distribution/keeper"
	distrtypes "github.com/cosmos/cosmos-sdk/x/distribution/types"
	govkeeper "github.com/cosmos/cosmos-sdk/x/gov/keeper"
	govtypes "github.com/cosmos/cosmos-sdk/x/gov/types"
	govv1 "github.com/cosmos/cosmos-sdk/x/gov/types/v1"
	govv1beta1 "github.com/cosmos/cosmos-sdk/x/gov/types/v1beta1"
	mintkeeper "github.com/cosmos/cosmos-sdk/x/mint/keeper"
	minttypes "github.com/cosmos/cosmos-sdk/x/mint/types"
	"github.com/cosmos/cosmos-sdk/x/params"
	paramskeeper "github.com/cosmos/cosmos-sdk/x/params/keeper"
	paramstypes "github.com/cosmos/cosmos-sdk/x/params/types"
	paramproposal "github.com/cosmos/cosmos-sdk/x/params/types/proposal"
	slashingkeeper "github.com/cosmos/cosmos-sdk/x/slashing/keeper"
	slashingtypes "github.com/cosmos/cosmos-sdk/x/slashing/types"
	stakingkeeper "github.com/cosmos/cosmos-sdk/x/staking/keeper"
	stakingtypes "github.com/cosmos/cosmos-sdk/x/staking/types"

	accountkeeper "github.com/perpdex/perpdex-l1/x/account/keeper"
	accounttypes "github.com/perpdex/perpdex-l1/x/account/types"
	assetkeeper "github.com/perpdex/perpdex-l1/x/asset/keeper"
	assettypes "github.com/perpdex/perpdex-l1/x/asset/types"
	fundingkeeper "github.com/perpdex/perpdex-l1/x/funding/keeper"
	fundingtypes "github.com/perpdex/perpdex-l1/x/funding/types"
	liquidationkeeper "github.com/perpdex/perpdex-l1/x/liquidation/keeper"
	liquidationtypes "github.com/perpdex/perpdex-l1/x/liquidation/types"
	marketkeeper "github.com/perpdex/perpdex-l1/x/market/keeper"
	markettypes "github.com/perpdex/perpdex-l1/x/market/types"
	matchingkeeper "github.com/perpdex/perpdex-l1/x/matching/keeper"
	matchingtypes "github.com/perpdex/perpdex-l1/x/matching/types"
	oraclekeeper "github.com/perpdex/perpdex-l1/x/oracle/keeper"
	oracletypes "github.com/perpdex/perpdex-l1/x/oracle/types"
	orderbookkeeper "github.com/perpdex/perpdex-l1/x/orderbook/keeper"
	orderbooktypes "github.com/perpdex/perpdex-l1/x/orderbook/types"
	riskkeeper "github.com/perpdex/perpdex-l1/x/risk/keeper"
	risktypes "github.com/perpdex/perpdex-l1/x/risk/types"
	tradekeeper "github.com/perpdex/perpdex-l1/x/trade/keeper"
	tradetypes "github.com/perpdex/perpdex-l1/x/trade/types"
)

// AppKeepers groups together every keeper used by PerpDEXApp so that they
// can be passed around as a single value (notably to upgrade handlers).
type AppKeepers struct {
	keys    map[string]*storetypes.KVStoreKey
	tkeys   map[string]*storetypes.TransientStoreKey
	memKeys map[string]*storetypes.MemoryStoreKey

	// SDK keepers
	AccountKeeper         authkeeper.AccountKeeper
	BankKeeper            bankkeeper.Keeper
	StakingKeeper         *stakingkeeper.Keeper
	SlashingKeeper        slashingkeeper.Keeper
	MintKeeper            mintkeeper.Keeper
	DistrKeeper           distrkeeper.Keeper
	GovKeeper             *govkeeper.Keeper
	UpgradeKeeper         *upgradekeeper.Keeper
	ParamsKeeper          paramskeeper.Keeper //nolint:staticcheck
	EvidenceKeeper        evidencekeeper.Keeper
	FeeGrantKeeper        feegrantkeeper.Keeper
	AuthzKeeper           authzkeeper.Keeper
	ConsensusParamsKeeper consensusparamkeeper.Keeper

	// IBC keepers
	IBCKeeper      *ibckeeper.Keeper
	TransferKeeper ibctransferkeeper.Keeper

	// IBC modules (so app.go can register them with the module manager)
	TransferModule transfer.AppModule

	// Perpdex L1 keepers
	AssetKeeper       assetkeeper.Keeper
	PerpAccountKeeper accountkeeper.Keeper
	MarketKeeper      marketkeeper.Keeper
	OracleKeeper      oraclekeeper.Keeper
	OrderbookKeeper   orderbookkeeper.Keeper
	FundingKeeper     fundingkeeper.Keeper
	RiskKeeper        riskkeeper.Keeper
	TradeKeeper       tradekeeper.Keeper
	MatchingKeeper    matchingkeeper.Keeper
	LiquidationKeeper liquidationkeeper.Keeper
}

// NewAppKeeper wires together every keeper required by PerpDEXApp.
//
// Keepers are constructed roughly in dependency order:
// params/consensus -> auth/bank/authz/feegrant -> staking/distr/slashing/mint
// -> upgrade -> ibc -> gov -> evidence -> ibc-transfer -> ibc router.
func NewAppKeeper(
	appCodec codec.Codec,
	bApp *baseapp.BaseApp,
	legacyAmino *codec.LegacyAmino,
	maccPerms map[string][]string,
	modAccAddrs map[string]bool,
	blockedAddress map[string]bool,
	skipUpgradeHeights map[int64]bool,
	homePath string,
	logger log.Logger,
	appOpts servertypes.AppOptions,
) AppKeepers {
	appKeepers := AppKeepers{}

	appKeepers.GenerateKeys()

	// Register state streaming services if enabled via app.toml.
	if err := bApp.RegisterStreamingServices(appOpts, appKeepers.keys); err != nil {
		logger.Error("failed to load state streaming", "err", err)
		os.Exit(1)
	}

	govModuleAddr := authtypes.NewModuleAddress(govtypes.ModuleName).String()

	appKeepers.ParamsKeeper = initParamsKeeper(
		appCodec,
		legacyAmino,
		appKeepers.keys[paramstypes.StoreKey],
		appKeepers.tkeys[paramstypes.TStoreKey],
	)

	appKeepers.ConsensusParamsKeeper = consensusparamkeeper.NewKeeper(
		appCodec,
		runtime.NewKVStoreService(appKeepers.keys[consensusparamtypes.StoreKey]),
		govModuleAddr,
		runtime.EventService{},
	)
	bApp.SetParamStore(appKeepers.ConsensusParamsKeeper.ParamsStore)

	appKeepers.AccountKeeper = authkeeper.NewAccountKeeper(
		appCodec,
		runtime.NewKVStoreService(appKeepers.keys[authtypes.StoreKey]),
		authtypes.ProtoBaseAccount,
		maccPerms,
		address.NewBech32Codec(sdk.GetConfig().GetBech32AccountAddrPrefix()),
		sdk.GetConfig().GetBech32AccountAddrPrefix(),
		govModuleAddr,
	)

	appKeepers.BankKeeper = bankkeeper.NewBaseKeeper(
		appCodec,
		runtime.NewKVStoreService(appKeepers.keys[banktypes.StoreKey]),
		appKeepers.AccountKeeper,
		blockedAddress,
		govModuleAddr,
		logger,
	)

	appKeepers.AuthzKeeper = authzkeeper.NewKeeper(
		runtime.NewKVStoreService(appKeepers.keys[authzkeeper.StoreKey]),
		appCodec,
		bApp.MsgServiceRouter(),
		appKeepers.AccountKeeper,
	).SetBankKeeper(appKeepers.BankKeeper)

	appKeepers.FeeGrantKeeper = feegrantkeeper.NewKeeper(
		appCodec,
		runtime.NewKVStoreService(appKeepers.keys[feegrant.StoreKey]),
		appKeepers.AccountKeeper,
	)

	appKeepers.StakingKeeper = stakingkeeper.NewKeeper(
		appCodec,
		runtime.NewKVStoreService(appKeepers.keys[stakingtypes.StoreKey]),
		appKeepers.AccountKeeper,
		appKeepers.BankKeeper,
		govModuleAddr,
		authcodec.NewBech32Codec(sdk.GetConfig().GetBech32ValidatorAddrPrefix()),
		authcodec.NewBech32Codec(sdk.GetConfig().GetBech32ConsensusAddrPrefix()),
	)

	appKeepers.DistrKeeper = distrkeeper.NewKeeper(
		appCodec,
		runtime.NewKVStoreService(appKeepers.keys[distrtypes.StoreKey]),
		appKeepers.AccountKeeper,
		appKeepers.BankKeeper,
		appKeepers.StakingKeeper,
		authtypes.FeeCollectorName,
		govModuleAddr,
	)

	appKeepers.SlashingKeeper = slashingkeeper.NewKeeper(
		appCodec,
		legacyAmino,
		runtime.NewKVStoreService(appKeepers.keys[slashingtypes.StoreKey]),
		appKeepers.StakingKeeper,
		govModuleAddr,
	)

	// Hooks must be wired only once StakingKeeper is fully constructed but
	// before any other keeper that depends on staking events runs.
	appKeepers.StakingKeeper.SetHooks(
		stakingtypes.NewMultiStakingHooks(
			appKeepers.DistrKeeper.Hooks(),
			appKeepers.SlashingKeeper.Hooks(),
		),
	)

	appKeepers.MintKeeper = mintkeeper.NewKeeper(
		appCodec,
		runtime.NewKVStoreService(appKeepers.keys[minttypes.StoreKey]),
		appKeepers.StakingKeeper,
		appKeepers.AccountKeeper,
		appKeepers.BankKeeper,
		authtypes.FeeCollectorName,
		govModuleAddr,
	)

	// UpgradeKeeper must be created before IBCKeeper.
	appKeepers.UpgradeKeeper = upgradekeeper.NewKeeper(
		skipUpgradeHeights,
		runtime.NewKVStoreService(appKeepers.keys[upgradetypes.StoreKey]),
		appCodec,
		homePath,
		bApp,
		govModuleAddr,
	)

	appKeepers.IBCKeeper = ibckeeper.NewKeeper(
		appCodec,
		runtime.NewKVStoreService(appKeepers.keys[ibcexported.StoreKey]),
		appKeepers.GetSubspace(ibcexported.ModuleName),
		appKeepers.UpgradeKeeper,
		govModuleAddr,
	)

	govConfig := govtypes.DefaultConfig()
	govConfig.MaxMetadataLen = 10200
	appKeepers.GovKeeper = govkeeper.NewKeeper(
		appCodec,
		runtime.NewKVStoreService(appKeepers.keys[govtypes.StoreKey]),
		appKeepers.AccountKeeper,
		appKeepers.BankKeeper,
		appKeepers.StakingKeeper,
		appKeepers.DistrKeeper,
		bApp.MsgServiceRouter(),
		govConfig,
		govModuleAddr,
	)

	// Register the legacy v1beta1 proposal router for backwards compatibility.
	govRouter := govv1beta1.NewRouter()
	govRouter.
		AddRoute(govtypes.RouterKey, govv1beta1.ProposalHandler).
		AddRoute(paramproposal.RouterKey, params.NewParamChangeProposalHandler(appKeepers.ParamsKeeper))
	appKeepers.GovKeeper.SetLegacyRouter(govRouter)

	evidenceKeeper := evidencekeeper.NewKeeper(
		appCodec,
		runtime.NewKVStoreService(appKeepers.keys[evidencetypes.StoreKey]),
		appKeepers.StakingKeeper,
		appKeepers.SlashingKeeper,
		appKeepers.AccountKeeper.AddressCodec(),
		runtime.ProvideCometInfoService(),
	)
	appKeepers.EvidenceKeeper = *evidenceKeeper

	appKeepers.TransferKeeper = ibctransferkeeper.NewKeeper(
		appCodec,
		runtime.NewKVStoreService(appKeepers.keys[ibctransfertypes.StoreKey]),
		appKeepers.GetSubspace(ibctransfertypes.ModuleName),
		appKeepers.IBCKeeper.ChannelKeeper, // ICS4Wrapper (no middleware)
		appKeepers.IBCKeeper.ChannelKeeper,
		bApp.MsgServiceRouter(),
		appKeepers.AccountKeeper,
		appKeepers.BankKeeper,
		govModuleAddr,
	)

	transferIBCModule := transfer.NewIBCModule(appKeepers.TransferKeeper)

	ibcRouter := porttypes.NewRouter().
		AddRoute(ibctransfertypes.ModuleName, transferIBCModule)
	appKeepers.IBCKeeper.SetRouter(ibcRouter)

	appKeepers.TransferModule = transfer.NewAppModule(appKeepers.TransferKeeper)

	// ---------- Perpdex L1 keepers ----------
	appKeepers.AssetKeeper = assetkeeper.NewKeeper(
		appCodec,
		runtime.NewKVStoreService(appKeepers.keys[assettypes.StoreKey]),
		govModuleAddr,
	)
	appKeepers.PerpAccountKeeper = accountkeeper.NewKeeper(
		appCodec,
		runtime.NewKVStoreService(appKeepers.keys[accounttypes.StoreKey]),
		govModuleAddr,
		appKeepers.AssetKeeper,
		appKeepers.BankKeeper,
	)
	appKeepers.MarketKeeper = marketkeeper.NewKeeper(
		appCodec,
		runtime.NewKVStoreService(appKeepers.keys[markettypes.StoreKey]),
		govModuleAddr,
		appKeepers.AssetKeeper,
	)
	appKeepers.OracleKeeper = oraclekeeper.NewKeeper(
		appCodec,
		runtime.NewKVStoreService(appKeepers.keys[oracletypes.StoreKey]),
		govModuleAddr,
		appKeepers.StakingKeeper,
		nil, // slashing-keeper hooks: skipped in MVP
	)
	appKeepers.OrderbookKeeper = orderbookkeeper.NewKeeper(
		appCodec,
		runtime.NewKVStoreService(appKeepers.keys[orderbooktypes.StoreKey]),
		govModuleAddr,
		appKeepers.MarketKeeper,
	)
	appKeepers.FundingKeeper = fundingkeeper.NewKeeper(
		appCodec,
		runtime.NewKVStoreService(appKeepers.keys[fundingtypes.StoreKey]),
		govModuleAddr,
		appKeepers.MarketKeeper,
		appKeepers.OracleKeeper,
		appKeepers.OrderbookKeeper,
		appKeepers.PerpAccountKeeper,
	)
	appKeepers.RiskKeeper = riskkeeper.NewKeeper(
		appCodec,
		runtime.NewKVStoreService(appKeepers.keys[risktypes.StoreKey]),
		govModuleAddr,
		appKeepers.PerpAccountKeeper,
		appKeepers.MarketKeeper,
		appKeepers.OracleKeeper,
	)
	appKeepers.TradeKeeper = tradekeeper.NewKeeper(
		appCodec,
		runtime.NewKVStoreService(appKeepers.keys[tradetypes.StoreKey]),
		govModuleAddr,
		appKeepers.PerpAccountKeeper,
		appKeepers.MarketKeeper,
		appKeepers.FundingKeeper,
		appKeepers.RiskKeeper,
	)
	appKeepers.MatchingKeeper = matchingkeeper.NewKeeper(
		appCodec,
		runtime.NewKVStoreService(appKeepers.keys[matchingtypes.StoreKey]),
		govModuleAddr,
		appKeepers.PerpAccountKeeper,
		appKeepers.MarketKeeper,
		appKeepers.OrderbookKeeper,
		appKeepers.TradeKeeper,
	)
	appKeepers.LiquidationKeeper = liquidationkeeper.NewKeeper(
		appCodec,
		runtime.NewKVStoreService(appKeepers.keys[liquidationtypes.StoreKey]),
		govModuleAddr,
		appKeepers.PerpAccountKeeper,
		appKeepers.MarketKeeper,
		appKeepers.RiskKeeper,
		appKeepers.TradeKeeper,
	)

	// Late wiring (break import cycles): account needs funding/risk hooks;
	// market needs the liquidation hook for expiry; ditto for funding.
	appKeepers.PerpAccountKeeper.SetFundingKeeper(appKeepers.FundingKeeper)
	appKeepers.PerpAccountKeeper.SetRiskKeeper(appKeepers.RiskKeeper)
	appKeepers.MarketKeeper.SetLiquidationKeeper(appKeepers.LiquidationKeeper)

	return appKeepers
}

// GetSubspace returns a param subspace for a given module name.
func (appKeepers *AppKeepers) GetSubspace(moduleName string) paramstypes.Subspace {
	subspace, ok := appKeepers.ParamsKeeper.GetSubspace(moduleName)
	if !ok {
		panic("couldn't load subspace for module: " + moduleName)
	}
	return subspace
}

// initParamsKeeper instantiates the legacy params keeper and registers the
// key tables for every module that still relies on it (mostly for genesis
// migration purposes; new chains may be free of these in the future).
func initParamsKeeper(
	appCodec codec.BinaryCodec,
	legacyAmino *codec.LegacyAmino,
	key, tkey storetypes.StoreKey,
) paramskeeper.Keeper { //nolint:staticcheck
	paramsKeeper := paramskeeper.NewKeeper(appCodec, legacyAmino, key, tkey) //nolint:staticcheck

	keyTable := ibcclienttypes.ParamKeyTable()
	keyTable.RegisterParamSet(&ibcconnectiontypes.Params{})

	// SDK v0.53 keeps the legacy `params` subspaces around so that modules
	// can serve old gRPC `Query/Params` requests during in-place migrations.
	// `WithKeyTable` is only kept for modules that still expose one in v0.53;
	// `mint` migrated fully to the new ParamStore so the subspace stays empty.
	paramsKeeper.Subspace(authtypes.ModuleName).WithKeyTable(authtypes.ParamKeyTable())         //nolint:staticcheck
	paramsKeeper.Subspace(banktypes.ModuleName).WithKeyTable(banktypes.ParamKeyTable())         //nolint:staticcheck
	paramsKeeper.Subspace(stakingtypes.ModuleName).WithKeyTable(stakingtypes.ParamKeyTable())   //nolint:staticcheck
	paramsKeeper.Subspace(distrtypes.ModuleName).WithKeyTable(distrtypes.ParamKeyTable())       //nolint:staticcheck
	paramsKeeper.Subspace(slashingtypes.ModuleName).WithKeyTable(slashingtypes.ParamKeyTable()) //nolint:staticcheck
	paramsKeeper.Subspace(govtypes.ModuleName).WithKeyTable(govv1.ParamKeyTable())              //nolint:staticcheck
	paramsKeeper.Subspace(minttypes.ModuleName)                                                 //nolint:staticcheck
	paramsKeeper.Subspace(ibcexported.ModuleName).WithKeyTable(keyTable)
	paramsKeeper.Subspace(ibctransfertypes.ModuleName).WithKeyTable(ibctransfertypes.ParamKeyTable())

	return paramsKeeper
}
