package app

import (
	sdk "github.com/cosmos/cosmos-sdk/types"

	perptypes "github.com/perpdex/perpdex-l1/types"
)

// SetAddressPrefixes configures the SDK Config with the chain's bech32
// prefixes. It is intentionally split out of init() so that callers (apps,
// CLI, tests) can decide when to apply and seal the configuration.
func SetAddressPrefixes() {
	cfg := sdk.GetConfig()
	cfg.SetBech32PrefixForAccount(perptypes.Bech32PrefixAccAddr, perptypes.Bech32PrefixAccPub)
	cfg.SetBech32PrefixForValidator(perptypes.Bech32PrefixValAddr, perptypes.Bech32PrefixValPub)
	cfg.SetBech32PrefixForConsensusNode(perptypes.Bech32PrefixConsAddr, perptypes.Bech32PrefixConsPub)
	cfg.SetCoinType(perptypes.CoinType)
}

func init() {
	SetAddressPrefixes()
}
