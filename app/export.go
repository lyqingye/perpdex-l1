package app

import (
	"encoding/json"
	"sort"

	tmproto "github.com/cometbft/cometbft/proto/tendermint/types"

	storetypes "cosmossdk.io/store/types"

	servertypes "github.com/cosmos/cosmos-sdk/server/types"
	sdk "github.com/cosmos/cosmos-sdk/types"
	slashingtypes "github.com/cosmos/cosmos-sdk/x/slashing/types"
	"github.com/cosmos/cosmos-sdk/x/staking"
	stakingtypes "github.com/cosmos/cosmos-sdk/x/staking/types"
)

// ExportAppStateAndValidators exports the application state and validator set
// at the current committed height for use as a fresh genesis.
func (app *PerpDEXApp) ExportAppStateAndValidators(
	forZeroHeight bool,
	jailAllowedAddrs []string,
	modulesToExport []string,
) (servertypes.ExportedApp, error) {
	ctx := app.NewContextLegacy(true, tmproto.Header{Height: app.LastBlockHeight()})

	height := app.LastBlockHeight() + 1
	if forZeroHeight {
		height = 0
		app.prepForZeroHeightGenesis(ctx, jailAllowedAddrs)
	}

	genState, err := app.mm.ExportGenesisForModules(ctx, app.appCodec, modulesToExport)
	if err != nil {
		return servertypes.ExportedApp{}, err
	}

	appState, err := json.MarshalIndent(genState, "", "  ")
	if err != nil {
		return servertypes.ExportedApp{}, err
	}

	validators, err := staking.WriteValidators(ctx, app.StakingKeeper)
	if err != nil {
		return servertypes.ExportedApp{}, err
	}
	sort.SliceStable(validators, func(i, j int) bool {
		return validators[i].Power > validators[j].Power
	})

	return servertypes.ExportedApp{
		AppState:        appState,
		Validators:      validators,
		Height:          height,
		ConsensusParams: app.GetConsensusParams(ctx),
	}, nil
}

// prepForZeroHeightGenesis prepares the chain state for export at height 0.
// This is mostly useful when migrating the chain to a new genesis file.
func (app *PerpDEXApp) prepForZeroHeightGenesis(ctx sdk.Context, jailAllowedAddrs []string) {
	applyAllowedAddrs := len(jailAllowedAddrs) > 0
	allowedAddrsMap := make(map[string]bool)
	for _, addr := range jailAllowedAddrs {
		if _, err := sdk.ValAddressFromBech32(addr); err != nil {
			panic(err)
		}
		allowedAddrsMap[addr] = true
	}

	// Withdraw all validator commissions.
	if err := app.StakingKeeper.IterateValidators(ctx, func(_ int64, val stakingtypes.ValidatorI) bool {
		valAddr, err := app.StakingKeeper.ValidatorAddressCodec().StringToBytes(val.GetOperator())
		if err != nil {
			app.Logger().Error(err.Error(), "ValOperatorAddress", val.GetOperator())
			return false
		}
		if _, err := app.DistrKeeper.WithdrawValidatorCommission(ctx, valAddr); err != nil {
			app.Logger().Error(err.Error(), "ValOperatorAddress", val.GetOperator())
		}
		return false
	}); err != nil {
		panic(err)
	}

	// Withdraw all delegator rewards.
	dels, err := app.StakingKeeper.GetAllDelegations(ctx)
	if err != nil {
		panic(err)
	}
	for _, delegation := range dels {
		valAddr, err := sdk.ValAddressFromBech32(delegation.ValidatorAddress)
		if err != nil {
			panic(err)
		}
		delAddr, err := sdk.AccAddressFromBech32(delegation.DelegatorAddress)
		if err != nil {
			panic(err)
		}
		if _, err := app.DistrKeeper.WithdrawDelegationRewards(ctx, delAddr, valAddr); err != nil {
			panic(err)
		}
	}

	app.DistrKeeper.DeleteAllValidatorSlashEvents(ctx)
	app.DistrKeeper.DeleteAllValidatorHistoricalRewards(ctx)

	height := ctx.BlockHeight()
	ctx = ctx.WithBlockHeight(0)

	// Reinitialize all validators / delegations against the zeroed context.
	if err := app.StakingKeeper.IterateValidators(ctx, func(_ int64, val stakingtypes.ValidatorI) bool {
		valAddr, err := app.StakingKeeper.ValidatorAddressCodec().StringToBytes(val.GetOperator())
		if err != nil {
			panic(err)
		}
		scraps, err := app.DistrKeeper.GetValidatorOutstandingRewardsCoins(ctx, valAddr)
		if err != nil {
			panic(err)
		}
		feePool, err := app.DistrKeeper.FeePool.Get(ctx)
		if err != nil {
			panic(err)
		}
		feePool.CommunityPool = feePool.CommunityPool.Add(scraps...)
		if err := app.DistrKeeper.FeePool.Set(ctx, feePool); err != nil {
			panic(err)
		}
		if err := app.DistrKeeper.Hooks().AfterValidatorCreated(ctx, valAddr); err != nil {
			panic(err)
		}
		return false
	}); err != nil {
		panic(err)
	}

	for _, del := range dels {
		valAddr, err := sdk.ValAddressFromBech32(del.ValidatorAddress)
		if err != nil {
			panic(err)
		}
		delAddr, err := sdk.AccAddressFromBech32(del.DelegatorAddress)
		if err != nil {
			panic(err)
		}
		if err := app.DistrKeeper.Hooks().BeforeDelegationCreated(ctx, delAddr, valAddr); err != nil {
			panic(err)
		}
		if err := app.DistrKeeper.Hooks().AfterDelegationModified(ctx, delAddr, valAddr); err != nil {
			panic(err)
		}
	}

	ctx = ctx.WithBlockHeight(height)

	if err := app.StakingKeeper.IterateRedelegations(ctx, func(_ int64, red stakingtypes.Redelegation) bool {
		for i := range red.Entries {
			red.Entries[i].CreationHeight = 0
		}
		if err := app.StakingKeeper.SetRedelegation(ctx, red); err != nil {
			panic(err)
		}
		return false
	}); err != nil {
		panic(err)
	}

	if err := app.StakingKeeper.IterateUnbondingDelegations(ctx, func(_ int64, ubd stakingtypes.UnbondingDelegation) bool {
		for i := range ubd.Entries {
			ubd.Entries[i].CreationHeight = 0
		}
		if err := app.StakingKeeper.SetUnbondingDelegation(ctx, ubd); err != nil {
			panic(err)
		}
		return false
	}); err != nil {
		panic(err)
	}

	store := ctx.KVStore(app.GetKey(stakingtypes.StoreKey))
	iter := storetypes.KVStoreReversePrefixIterator(store, stakingtypes.ValidatorsKey)
	func() {
		defer iter.Close()
		for ; iter.Valid(); iter.Next() {
			addr := sdk.ValAddress(stakingtypes.AddressFromValidatorsKey(iter.Key()))
			validator, err := app.StakingKeeper.GetValidator(ctx, addr)
			if err != nil {
				panic("expected validator, not found")
			}
			validator.UnbondingHeight = 0
			if applyAllowedAddrs && !allowedAddrsMap[addr.String()] {
				validator.Jailed = true
			}
			if err := app.StakingKeeper.SetValidator(ctx, validator); err != nil {
				panic(err)
			}
		}
	}()

	if _, err := app.StakingKeeper.ApplyAndReturnValidatorSetUpdates(ctx); err != nil {
		panic(err)
	}

	if err := app.SlashingKeeper.IterateValidatorSigningInfos(
		ctx,
		func(addr sdk.ConsAddress, info slashingtypes.ValidatorSigningInfo) bool {
			info.StartHeight = 0
			if err := app.SlashingKeeper.SetValidatorSigningInfo(ctx, addr, info); err != nil {
				panic(err)
			}
			return false
		},
	); err != nil {
		panic(err)
	}
}
