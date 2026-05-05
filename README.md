# PerpDEX L1

PerpDEX L1 is a Cosmos SDK blockchain for the PerpDEX protocol. It keeps the
base chain small while adding the application modules needed for perpetual
futures trading, margin/risk accounting, public liquidity pools, liquidation and
auto-deleveraging.

| Property      | Value        |
| ------------- | ------------ |
| App name      | `PerpDEXApp` |
| Binary        | `perpd`      |
| Native denom  | `uperp`      |
| Bech32 prefix | `px`         |
| Default home  | `~/.perpd`   |

## Features

- Cosmos SDK application with governance, staking, bank, authz, feegrant,
  upgrade and IBC transfer support.
- PerpDEX modules for assets, markets, oracle prices, accounts, matching,
  orderbook, trades, funding, risk and liquidation.
- Master and sub-account model with collateral deposits, withdrawals, leverage
  updates and trading-mode bookkeeping.
- Perpetual market flow with asset registration, oracle price injection, order
  placement, matching and position settlement.
- Risk engine helpers for health status, available collateral, total account
  value, position zero price and unrealized PnL.
- Liquidation flow aligned with the
  specification: 5-tier health classification (HEALTHY / PRE / PARTIAL / FULL /
  BANKRUPTCY), pre-liquidation order placement gate (reduce-only in PRE; all
  user orders blocked in PARTIAL+), mark-based zero-price IoC close-outs with
  improvement-over-zero-price fees routed to the LLP (capped at 1%), LLP
  takeover ranked by ascending uPnL with per-position IMR safety check, and
  fall-through ADL ranked by leverage × uPnL ratio with double-sided zero-price
  alignment.
- Public pool and insurance fund support: create/update public pools, mint and
  burn LP shares, NAV-based share valuation, operator fee shares, LLP burn
  cooldowns, strategy bucket transfers and governance force-burn.
- gRPC and REST query surfaces generated from protobuf definitions for accounts,
  positions, public pool info/shares/share value, liquidation flags and ADL
  candidates.
- Integration and e2e test coverage for genesis, funding, oracle, trading,
  market expiry, liquidation, ADL and public-pool behavior.

## Modules

Custom PerpDEX modules:

- `x/asset`
- `x/market`
- `x/oracle`
- `x/account`
- `x/orderbook`
- `x/matching`
- `x/trade`
- `x/funding`
- `x/risk`
- `x/liquidation`

Standard Cosmos SDK modules include `auth`, `vesting`, `bank`, `staking`,
`mint`, `distribution`, `slashing`, `gov` v1, legacy `params`, `consensus`,
`upgrade`, `evidence`, `feegrant`, `authz` and `genutil`.

IBC support includes `ibc-go` core, the `07-tendermint` light client and ICS-20
`ibc-transfer`.

## Build

Use the Go version declared in `go.mod`. Docker is required for protobuf
generation targets because they run through the Cosmos proto-builder image.

```bash
make build           # builds ./build/perpd
make install         # installs perpd into $GOPATH/bin
make build-sidecar   # builds ./oracle-sidecar/build/oracle-sidecar
make install-sidecar # installs the sidecar into $GOPATH/bin
make dev-stack       # builds both binaries, runs the sidecar in the
                     #   foreground and prints a reminder to start
                     #   `perpd` in another shell with [oracle].enabled=true
make clean           # removes build artifacts
make tidy            # runs go mod tidy
```

## Running validators with the oracle

The chain ships a dydx/Slinky-style ABCI++ vote-extension price feed. Each
validator is expected to run the [`oracle-sidecar`](./oracle-sidecar)
binary alongside `perpd`; the sidecar streams prices from Binance, OKX
and CoinGecko, exposes them on gRPC, and the chain daemon relays them
into vote extensions and PreBlock aggregation. See
[`docs/oracle/README.md`](./docs/oracle/README.md) for architecture, the
five-step quick start, and reference deployment examples.

## Test And Lint

```bash
make test              # go test ./...
make test-unit         # app, ante, cmd and shared type tests
make test-integration  # tests/integration
make test-race         # race-enabled Go tests
make lint              # golangci-lint over ./...
make proto-lint        # buf lint through proto-builder
```

The GitHub workflows mirror the local checks:

- `.github/workflows/test.yml` runs module-tidy verification and `make test`.
- `.github/workflows/lint.yml` runs `golangci-lint` with `.golangci.yml`.
- `.github/workflows/release.yml` runs tests, builds Linux and macOS archives
  for `amd64` and `arm64`, uploads checksums, and publishes them to a GitHub
  Release when a `v*` tag is pushed.

## Protobuf

```bash
make proto-gen          # regenerates protobuf Go code
make proto-swagger-gen  # regenerates Swagger output
make proto-format       # formats .proto files
make proto-all          # format, lint and generate protobuf code
```

## Quick Start

```bash
perpd init mynode --chain-id perpdex-1
perpd keys add validator
perpd genesis add-genesis-account validator 1000000000uperp
perpd genesis gentx validator 1000000uperp --chain-id perpdex-1
perpd genesis collect-gentxs
perpd start
```
