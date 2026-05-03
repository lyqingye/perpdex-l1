package app

import (
	ibctransfertypes "github.com/cosmos/ibc-go/v10/modules/apps/transfer/types"
	ibc "github.com/cosmos/ibc-go/v10/modules/core"
	ibcexported "github.com/cosmos/ibc-go/v10/modules/core/exported"
	tendermint "github.com/cosmos/ibc-go/v10/modules/light-clients/07-tendermint"

	"cosmossdk.io/x/evidence"
	evidencetypes "cosmossdk.io/x/evidence/types"
	"cosmossdk.io/x/feegrant"
	feegrantmodule "cosmossdk.io/x/feegrant/module"
	"cosmossdk.io/x/upgrade"
	upgradetypes "cosmossdk.io/x/upgrade/types"

	"github.com/cosmos/cosmos-sdk/client"
	"github.com/cosmos/cosmos-sdk/codec"
	"github.com/cosmos/cosmos-sdk/types/module"
	"github.com/cosmos/cosmos-sdk/x/auth"
	authsims "github.com/cosmos/cosmos-sdk/x/auth/simulation"
	authtypes "github.com/cosmos/cosmos-sdk/x/auth/types"
	"github.com/cosmos/cosmos-sdk/x/auth/vesting"
	vestingtypes "github.com/cosmos/cosmos-sdk/x/auth/vesting/types"
	"github.com/cosmos/cosmos-sdk/x/authz"
	authzmodule "github.com/cosmos/cosmos-sdk/x/authz/module"
	"github.com/cosmos/cosmos-sdk/x/bank"
	banktypes "github.com/cosmos/cosmos-sdk/x/bank/types"
	"github.com/cosmos/cosmos-sdk/x/consensus"
	consensusparamtypes "github.com/cosmos/cosmos-sdk/x/consensus/types"
	distr "github.com/cosmos/cosmos-sdk/x/distribution"
	distrtypes "github.com/cosmos/cosmos-sdk/x/distribution/types"
	"github.com/cosmos/cosmos-sdk/x/genutil"
	genutiltypes "github.com/cosmos/cosmos-sdk/x/genutil/types"
	"github.com/cosmos/cosmos-sdk/x/gov"
	govclient "github.com/cosmos/cosmos-sdk/x/gov/client"
	govtypes "github.com/cosmos/cosmos-sdk/x/gov/types"
	"github.com/cosmos/cosmos-sdk/x/mint"
	minttypes "github.com/cosmos/cosmos-sdk/x/mint/types"
	sdkparams "github.com/cosmos/cosmos-sdk/x/params"
	paramsclient "github.com/cosmos/cosmos-sdk/x/params/client"
	paramstypes "github.com/cosmos/cosmos-sdk/x/params/types"
	"github.com/cosmos/cosmos-sdk/x/slashing"
	slashingtypes "github.com/cosmos/cosmos-sdk/x/slashing/types"
	"github.com/cosmos/cosmos-sdk/x/staking"
	stakingtypes "github.com/cosmos/cosmos-sdk/x/staking/types"

	"github.com/perpdex/perpdex-l1/x/account"
	accounttypes "github.com/perpdex/perpdex-l1/x/account/types"
	"github.com/perpdex/perpdex-l1/x/asset"
	assettypes "github.com/perpdex/perpdex-l1/x/asset/types"
	"github.com/perpdex/perpdex-l1/x/funding"
	fundingtypes "github.com/perpdex/perpdex-l1/x/funding/types"
	"github.com/perpdex/perpdex-l1/x/liquidation"
	liquidationtypes "github.com/perpdex/perpdex-l1/x/liquidation/types"
	"github.com/perpdex/perpdex-l1/x/market"
	markettypes "github.com/perpdex/perpdex-l1/x/market/types"
	"github.com/perpdex/perpdex-l1/x/matching"
	matchingtypes "github.com/perpdex/perpdex-l1/x/matching/types"
	"github.com/perpdex/perpdex-l1/x/oracle"
	oracletypes "github.com/perpdex/perpdex-l1/x/oracle/types"
	"github.com/perpdex/perpdex-l1/x/orderbook"
	orderbooktypes "github.com/perpdex/perpdex-l1/x/orderbook/types"
	"github.com/perpdex/perpdex-l1/x/risk"
	risktypes "github.com/perpdex/perpdex-l1/x/risk/types"
	"github.com/perpdex/perpdex-l1/x/trade"
	tradetypes "github.com/perpdex/perpdex-l1/x/trade/types"
)

// maccPerms describes the permissions granted to module accounts. The
// PerpDEX chain only uses the SDK-default set of accounts plus IBC transfer.
var maccPerms = map[string][]string{
	authtypes.FeeCollectorName:     nil,
	distrtypes.ModuleName:          nil,
	minttypes.ModuleName:           {authtypes.Minter},
	stakingtypes.BondedPoolName:    {authtypes.Burner, authtypes.Staking},
	stakingtypes.NotBondedPoolName: {authtypes.Burner, authtypes.Staking},
	govtypes.ModuleName:            {authtypes.Burner},
	ibctransfertypes.ModuleName:    {authtypes.Minter, authtypes.Burner},
	// Perpdex module accounts.
	accounttypes.ModuleName: {authtypes.Minter, authtypes.Burner},
}

func appModules(
	app *PerpDEXApp,
	appCodec codec.Codec,
	txConfig client.TxEncodingConfig,
	tmLightClientModule tendermint.LightClientModule,
) []module.AppModule {
	return []module.AppModule{
		genutil.NewAppModule(app.AccountKeeper, app.StakingKeeper, app, txConfig),
		auth.NewAppModule(appCodec, app.AccountKeeper, nil, app.GetSubspace(authtypes.ModuleName)),
		vesting.NewAppModule(app.AccountKeeper, app.BankKeeper),
		bank.NewAppModule(appCodec, app.BankKeeper, app.AccountKeeper, app.GetSubspace(banktypes.ModuleName)),
		gov.NewAppModule(appCodec, app.GovKeeper, app.AccountKeeper, app.BankKeeper, app.GetSubspace(govtypes.ModuleName)),
		mint.NewAppModule(appCodec, app.MintKeeper, app.AccountKeeper, nil, app.GetSubspace(minttypes.ModuleName)),
		slashing.NewAppModule(appCodec, app.SlashingKeeper, app.AccountKeeper, app.BankKeeper, app.StakingKeeper, app.GetSubspace(slashingtypes.ModuleName), app.interfaceRegistry),
		distr.NewAppModule(appCodec, app.DistrKeeper, app.AccountKeeper, app.BankKeeper, app.StakingKeeper, app.GetSubspace(distrtypes.ModuleName)),
		staking.NewAppModule(appCodec, app.StakingKeeper, app.AccountKeeper, app.BankKeeper, app.GetSubspace(stakingtypes.ModuleName)),
		upgrade.NewAppModule(app.UpgradeKeeper, app.AccountKeeper.AddressCodec()),
		evidence.NewAppModule(app.EvidenceKeeper),
		feegrantmodule.NewAppModule(appCodec, app.AccountKeeper, app.BankKeeper, app.FeeGrantKeeper, app.interfaceRegistry),
		authzmodule.NewAppModule(appCodec, app.AuthzKeeper, app.AccountKeeper, app.BankKeeper, app.interfaceRegistry),
		ibc.NewAppModule(app.IBCKeeper),
		sdkparams.NewAppModule(app.ParamsKeeper), //nolint:staticcheck
		consensus.NewAppModule(appCodec, app.ConsensusParamsKeeper),
		app.TransferModule,
		tendermint.NewAppModule(tmLightClientModule),

		// Perpdex L1 modules.
		asset.NewAppModule(appCodec, app.AssetKeeper),
		account.NewAppModule(appCodec, app.PerpAccountKeeper),
		market.NewAppModule(appCodec, app.MarketKeeper),
		oracle.NewAppModule(appCodec, app.OracleKeeper),
		orderbook.NewAppModule(appCodec, app.OrderbookKeeper),
		funding.NewAppModule(appCodec, app.FundingKeeper),
		risk.NewAppModule(appCodec, app.RiskKeeper),
		trade.NewAppModule(appCodec, app.TradeKeeper),
		matching.NewAppModule(appCodec, app.MatchingKeeper),
		liquidation.NewAppModule(appCodec, app.LiquidationKeeper),
	}
}

// newBasicManagerFromManager creates the BasicManager from the module manager,
// applying minimal overrides for genesis validators / proposal handlers.
func newBasicManagerFromManager(app *PerpDEXApp) module.BasicManager {
	basicManager := module.NewBasicManagerFromManager(
		app.mm,
		map[string]module.AppModuleBasic{
			genutiltypes.ModuleName: genutil.NewAppModuleBasic(genutiltypes.DefaultMessageValidator),
			govtypes.ModuleName: gov.NewAppModuleBasic(
				[]govclient.ProposalHandler{
					paramsclient.ProposalHandler,
				},
			),
		})
	basicManager.RegisterLegacyAminoCodec(app.legacyAmino)
	basicManager.RegisterInterfaces(app.interfaceRegistry)
	return basicManager
}

// simulationModules returns the modules that participate in deterministic
// simulation runs.
func simulationModules(
	app *PerpDEXApp,
	appCodec codec.Codec,
) []module.AppModuleSimulation {
	return []module.AppModuleSimulation{
		auth.NewAppModule(appCodec, app.AccountKeeper, authsims.RandomGenesisAccounts, app.GetSubspace(authtypes.ModuleName)),
		bank.NewAppModule(appCodec, app.BankKeeper, app.AccountKeeper, app.GetSubspace(banktypes.ModuleName)),
		feegrantmodule.NewAppModule(appCodec, app.AccountKeeper, app.BankKeeper, app.FeeGrantKeeper, app.interfaceRegistry),
		gov.NewAppModule(appCodec, app.GovKeeper, app.AccountKeeper, app.BankKeeper, app.GetSubspace(govtypes.ModuleName)),
		mint.NewAppModule(appCodec, app.MintKeeper, app.AccountKeeper, nil, app.GetSubspace(minttypes.ModuleName)),
		staking.NewAppModule(appCodec, app.StakingKeeper, app.AccountKeeper, app.BankKeeper, app.GetSubspace(stakingtypes.ModuleName)),
		distr.NewAppModule(appCodec, app.DistrKeeper, app.AccountKeeper, app.BankKeeper, app.StakingKeeper, app.GetSubspace(distrtypes.ModuleName)),
		slashing.NewAppModule(appCodec, app.SlashingKeeper, app.AccountKeeper, app.BankKeeper, app.StakingKeeper, app.GetSubspace(slashingtypes.ModuleName), app.interfaceRegistry),
		evidence.NewAppModule(app.EvidenceKeeper),
		authzmodule.NewAppModule(appCodec, app.AuthzKeeper, app.AccountKeeper, app.BankKeeper, app.interfaceRegistry),
		ibc.NewAppModule(app.IBCKeeper),
		app.TransferModule,
	}
}

func orderBeginBlockers() []string {
	return []string{
		minttypes.ModuleName,
		distrtypes.ModuleName,
		slashingtypes.ModuleName,
		evidencetypes.ModuleName,
		stakingtypes.ModuleName,
		authtypes.ModuleName,
		banktypes.ModuleName,
		govtypes.ModuleName,
		ibcexported.ModuleName,
		ibctransfertypes.ModuleName,
		genutiltypes.ModuleName,
		authz.ModuleName,
		feegrant.ModuleName,
		paramstypes.ModuleName,
		vestingtypes.ModuleName,
		consensusparamtypes.ModuleName,
		// Perpdex begin blockers (oracle -> funding -> market scan).
		oracletypes.ModuleName,
		fundingtypes.ModuleName,
		markettypes.ModuleName,
	}
}

func orderEndBlockers() []string {
	return []string{
		govtypes.ModuleName,
		stakingtypes.ModuleName,
		ibcexported.ModuleName,
		ibctransfertypes.ModuleName,
		authtypes.ModuleName,
		banktypes.ModuleName,
		distrtypes.ModuleName,
		slashingtypes.ModuleName,
		minttypes.ModuleName,
		genutiltypes.ModuleName,
		evidencetypes.ModuleName,
		authz.ModuleName,
		feegrant.ModuleName,
		paramstypes.ModuleName,
		upgradetypes.ModuleName,
		vestingtypes.ModuleName,
		consensusparamtypes.ModuleName,
		// Perpdex end blockers: market expires first, orderbook GTT cleanup,
		// matching trigger scan, liquidation, oracle stats.
		markettypes.ModuleName,
		orderbooktypes.ModuleName,
		matchingtypes.ModuleName,
		liquidationtypes.ModuleName,
		oracletypes.ModuleName,
	}
}

func orderInitBlockers() []string {
	return []string{
		authtypes.ModuleName,
		banktypes.ModuleName,
		distrtypes.ModuleName,
		stakingtypes.ModuleName,
		slashingtypes.ModuleName,
		govtypes.ModuleName,
		minttypes.ModuleName,
		genutiltypes.ModuleName,
		ibcexported.ModuleName,
		ibctransfertypes.ModuleName,
		evidencetypes.ModuleName,
		authz.ModuleName,
		feegrant.ModuleName,
		paramstypes.ModuleName,
		upgradetypes.ModuleName,
		vestingtypes.ModuleName,
		consensusparamtypes.ModuleName,
		// Perpdex InitGenesis order: asset -> account -> market -> oracle ->
		// orderbook -> funding -> risk -> trade -> matching -> liquidation.
		assettypes.ModuleName,
		accounttypes.ModuleName,
		markettypes.ModuleName,
		oracletypes.ModuleName,
		orderbooktypes.ModuleName,
		fundingtypes.ModuleName,
		risktypes.ModuleName,
		tradetypes.ModuleName,
		matchingtypes.ModuleName,
		liquidationtypes.ModuleName,
	}
}
