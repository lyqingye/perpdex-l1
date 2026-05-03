// Package helpers exposes utilities for spinning up an in-process PerpDEXApp
// instance suitable for unit / integration tests. Anything that needs a
// running app + ABCI chain (rather than just a single keeper) should depend
// on these helpers instead of constructing the app manually.
package helpers

import (
	"encoding/json"
	"os"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	abci "github.com/cometbft/cometbft/abci/types"
	tmproto "github.com/cometbft/cometbft/proto/tendermint/types"
	tmtypes "github.com/cometbft/cometbft/types"

	dbm "github.com/cosmos/cosmos-db"

	"cosmossdk.io/log"
	"cosmossdk.io/math"

	"github.com/cosmos/cosmos-sdk/baseapp"
	codectypes "github.com/cosmos/cosmos-sdk/codec/types"
	cryptocodec "github.com/cosmos/cosmos-sdk/crypto/codec"
	cryptotypes "github.com/cosmos/cosmos-sdk/crypto/types"
	"github.com/cosmos/cosmos-sdk/server"
	"github.com/cosmos/cosmos-sdk/testutil/mock"
	simtestutil "github.com/cosmos/cosmos-sdk/testutil/sims"
	sdk "github.com/cosmos/cosmos-sdk/types"
	authtypes "github.com/cosmos/cosmos-sdk/x/auth/types"
	banktypes "github.com/cosmos/cosmos-sdk/x/bank/types"
	stakingtypes "github.com/cosmos/cosmos-sdk/x/staking/types"

	perp "github.com/perpdex/perpdex-l1/app"
	perptypes "github.com/perpdex/perpdex-l1/types"
)

// SimAppChainID is the chain ID used by every helper in this package.
const SimAppChainID = "perpdex-app"

// DefaultConsensusParams are sensible defaults for tests that need to create
// the consensus state at genesis time.
var DefaultConsensusParams = &tmproto.ConsensusParams{
	Block: &tmproto.BlockParams{
		MaxBytes: 200000,
		MaxGas:   2000000,
	},
	Evidence: &tmproto.EvidenceParams{
		MaxAgeNumBlocks: 302400,
		MaxAgeDuration:  504 * time.Hour,
		MaxBytes:        10000,
	},
	Validator: &tmproto.ValidatorParams{
		PubKeyTypes: []string{tmtypes.ABCIPubKeyTypeEd25519},
	},
}

// PV is a tiny holder for a private validator key; useful when tests want to
// drive consensus directly.
type PV struct {
	PrivKey cryptotypes.PrivKey
}

// EmptyAppOptions implements `servertypes.AppOptions` and always returns nil.
type EmptyAppOptions struct{}

func (EmptyAppOptions) Get(_ string) interface{} { return nil }

// Setup builds a PerpDEXApp with a single bonded validator and a single
// genesis account that holds 100T uperp; perfect for happy-path tests.
func Setup(t *testing.T) *perp.PerpDEXApp {
	t.Helper()

	privVal := mock.NewPV()
	pubKey, err := privVal.GetPubKey()
	require.NoError(t, err)
	validator := tmtypes.NewValidator(pubKey, 1)
	valSet := tmtypes.NewValidatorSet([]*tmtypes.Validator{validator})

	senderPrivKey := mock.NewPV()
	senderPubKey := senderPrivKey.PrivKey.PubKey()
	acc := authtypes.NewBaseAccount(senderPubKey.Address().Bytes(), senderPubKey, 0, 0)
	balance := banktypes.Balance{
		Address: acc.GetAddress().String(),
		Coins:   sdk.NewCoins(sdk.NewCoin(perptypes.UPerpDenom, math.NewInt(100_000_000_000_000))),
	}

	return SetupWithGenesisValSet(t, valSet, []authtypes.GenesisAccount{acc}, balance)
}

// SetupWithGenesisValSet boots a PerpDEXApp with a caller-supplied validator
// set and genesis accounts. The first account also acts as the delegator for
// the bonded validators.
func SetupWithGenesisValSet(
	t *testing.T,
	valSet *tmtypes.ValidatorSet,
	genAccs []authtypes.GenesisAccount,
	balances ...banktypes.Balance,
) *perp.PerpDEXApp {
	t.Helper()

	app, genesisState := setup()
	genesisState = genesisStateWithValSet(t, app, genesisState, valSet, genAccs, balances...)

	stateBytes, err := json.MarshalIndent(genesisState, "", " ")
	require.NoError(t, err)

	_, err = app.InitChain(
		&abci.RequestInitChain{
			ChainId:         SimAppChainID,
			Validators:      []abci.ValidatorUpdate{},
			ConsensusParams: DefaultConsensusParams,
			AppStateBytes:   stateBytes,
		},
	)
	require.NoError(t, err)

	_, err = app.FinalizeBlock(&abci.RequestFinalizeBlock{
		Height:             app.LastBlockHeight() + 1,
		Hash:               app.LastCommitID().Hash,
		NextValidatorsHash: valSet.Hash(),
	})
	require.NoError(t, err)

	return app
}

// setup builds a fresh PerpDEXApp backed by an in-memory DB plus the empty
// default genesis state. Callers that want the pre-bonded fixture should use
// `Setup` or `SetupWithGenesisValSet` instead.
func setup() (*perp.PerpDEXApp, perp.GenesisState) {
	db := dbm.NewMemDB()
	dir, err := os.MkdirTemp("", "perpdex-test-app")
	if err != nil {
		panic(err)
	}

	appOptions := make(simtestutil.AppOptionsMap)
	appOptions[server.FlagInvCheckPeriod] = 5
	appOptions[server.FlagMinGasPrices] = "0" + perptypes.UPerpDenom

	app := perp.NewPerpDEXApp(
		log.NewNopLogger(),
		db,
		nil,
		true,
		map[int64]bool{},
		dir,
		appOptions,
		baseapp.SetChainID(SimAppChainID),
	)
	return app, app.ModuleBasics.DefaultGenesis(app.AppCodec())
}

// genesisStateWithValSet patches the auth, staking and bank genesis sections
// so that the supplied validator set is bonded and the supplied balances are
// reflected in total supply / module accounts.
func genesisStateWithValSet(
	t *testing.T,
	app *perp.PerpDEXApp,
	genesisState perp.GenesisState,
	valSet *tmtypes.ValidatorSet,
	genAccs []authtypes.GenesisAccount,
	balances ...banktypes.Balance,
) perp.GenesisState {
	t.Helper()

	authGenesis := authtypes.NewGenesisState(authtypes.DefaultParams(), genAccs)
	genesisState[authtypes.ModuleName] = app.AppCodec().MustMarshalJSON(authGenesis)

	validators := make([]stakingtypes.Validator, 0, len(valSet.Validators))
	delegations := make([]stakingtypes.Delegation, 0, len(valSet.Validators))

	bondAmt := sdk.DefaultPowerReduction
	bondDenom := perptypes.UPerpDenom

	for _, val := range valSet.Validators {
		pk, err := cryptocodec.FromCmtPubKeyInterface(val.PubKey)
		require.NoError(t, err)
		pkAny, err := codectypes.NewAnyWithValue(pk)
		require.NoError(t, err)

		validator := stakingtypes.Validator{
			OperatorAddress: sdk.ValAddress(val.Address).String(),
			ConsensusPubkey: pkAny,
			Jailed:          false,
			Status:          stakingtypes.Bonded,
			Tokens:          bondAmt,
			DelegatorShares: math.LegacyOneDec(),
			Description:     stakingtypes.Description{},
			UnbondingHeight: 0,
			UnbondingTime:   time.Unix(0, 0).UTC(),
			Commission:      stakingtypes.NewCommission(math.LegacyZeroDec(), math.LegacyZeroDec(), math.LegacyZeroDec()),
		}
		validators = append(validators, validator)
		delegations = append(delegations,
			stakingtypes.NewDelegation(
				genAccs[0].GetAddress().String(),
				sdk.ValAddress(val.Address).String(),
				math.LegacyOneDec(),
			),
		)
	}

	stakingParams := stakingtypes.DefaultParams()
	stakingParams.BondDenom = bondDenom
	stakingGenesis := stakingtypes.NewGenesisState(stakingParams, validators, delegations)
	genesisState[stakingtypes.ModuleName] = app.AppCodec().MustMarshalJSON(stakingGenesis)

	totalSupply := sdk.NewCoins()
	for _, b := range balances {
		totalSupply = totalSupply.Add(b.Coins...)
	}
	for range delegations {
		totalSupply = totalSupply.Add(sdk.NewCoin(bondDenom, bondAmt))
	}

	balances = append(balances, banktypes.Balance{
		Address: authtypes.NewModuleAddress(stakingtypes.BondedPoolName).String(),
		Coins:   sdk.NewCoins(sdk.NewCoin(bondDenom, bondAmt)),
	})

	bankParams := banktypes.DefaultGenesisState().Params
	bankGenesis := banktypes.NewGenesisState(
		bankParams,
		balances,
		totalSupply,
		[]banktypes.Metadata{},
		[]banktypes.SendEnabled{},
	)
	genesisState[banktypes.ModuleName] = app.AppCodec().MustMarshalJSON(bankGenesis)

	return genesisState
}
