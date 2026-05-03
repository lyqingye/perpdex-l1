# PerpDEX L1

A Cosmos SDK based blockchain for the PerpDEX protocol. Inspired by the Cosmos Hub
([gaia](https://github.com/cosmos/gaia)) but slimmed down to a minimal IBC-enabled
chain.


| Property      | Value        |
| ------------- | ------------ |
| App name      | `PerpDEXApp` |
| Binary        | `perpd`      |
| Native denom  | `uperp`      |
| Bech32 prefix | `px`         |
| Default home  | `~/.perpd`   |


## Modules

Standard Cosmos SDK modules: `auth`, `vesting`, `bank`, `staking`, `mint`,
`distribution`, `slashing`, `gov` (v1), `params` (legacy), `consensus`, `upgrade`,
`evidence`, `feegrant`, `authz`, `genutil`.

IBC: `ibc-go` core, `07-tendermint` light client, ICS-20 (`ibc-transfer`).

## Build

```bash
make build         # produces ./build/perpd
make install       # installs perpd into $GOPATH/bin
```

## Quick start

```bash
perpd init mynode --chain-id perpdex-1
perpd keys add validator
perpd genesis add-genesis-account validator 1000000000uperp
perpd genesis gentx validator 1000000uperp --chain-id perpdex-1
perpd genesis collect-gentxs
perpd start
```

