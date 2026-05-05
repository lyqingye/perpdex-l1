# Perpdex Oracle

The perpdex-l1 oracle is a dydx/Slinky-style price feed pipeline. It is split
into two cooperating processes per validator:

- `oracle-sidecar`, an independent Go binary that streams prices from
  Binance, OKX and CoinGecko and exposes them on `Oracle.Prices` over gRPC.
- The on-chain `x/oracle` module, which runs a goroutine inside the chain
  process to poll the local sidecar, broadcasts prices via ABCI++ vote
  extensions, and persists per-block aggregates via PreBlock.

## Architecture

```
┌────────────────────────────┐    ┌────────────────────────────────────────┐
│   Binance / OKX / CoinGecko │   │              perpd validator           │
│        public REST APIs     │   │                                        │
└────────────┬────────────────┘   │  ┌───────────────────────────────────┐ │
             │ poll 1.5s          │  │ x/oracle/daemon                   │ │
             │                    │  │   ticker 500ms                    │ │
       ┌─────▼──────┐ gRPC :8080  │  │   priceCache map[uint32]CachedPrice│ │
       │ oracle-    ├─────────────┼──►   resolver  CurrencyPair → idx   │ │
       │ sidecar    │             │  └────────┬──────────────────────────┘ │
       │  process   │             │           │ FetchPrices                │
       └────────────┘             │  ┌────────▼─────────────────────────┐  │
       prom :8002                 │  │ ABCI++ pipeline                  │  │
                                  │  │  ExtendVote → VoteExtension      │  │
                                  │  │  PrepareProposal → Txs[0] = ExtCommit │ │
                                  │  │  ProcessProposal → 2/3+ check    │  │
                                  │  │  PreBlock → weighted median →    │  │
                                  │  │             OraclePrice store    │  │
                                  │  └──────────────────────────────────┘  │
                                  └────────────────────────────────────────┘
```

The full lifecycle of a single block:

1. **Sidecar (continuous)** — fetches prices from upstream exchanges, runs an
   internal cross-provider median, exposes `Oracle.Prices` and Prometheus
   metrics.
2. **Daemon (continuous)** — polls the sidecar every 500 ms, maps the
   returned `BTC/USD` strings to `market_index` via the markets+assets
   keepers, and stores them in an in-memory cache.
3. **ExtendVote** — every validator's local node reads the cache and
   marshals an `OracleVote` containing one `(market_index, index, mark)`
   tuple per market it tracks.
4. **PrepareProposal** — the proposer of height H takes
   `req.LocalLastCommit` (the cometbft `ExtendedCommitInfo` for height
   H-1), encodes it (zstd-wrapped), and prepends the bytes as `Txs[0]` of
   the new block proposal.
5. **ProcessProposal** — every validator decodes `Txs[0]`, runs
   `ValidateExtendedCommit` (2/3+ supermajority, schema check), and only
   then defers to the wrapped default handler.
6. **PreBlock** — every validator decodes `Txs[0]` again and runs the
   stake-weighted median per market, applies the optional EMA smoothing,
   and writes the result to the `OraclePrice` store via
   `Keeper.SetPrice(...)`.

## Quick start (single validator dev stack)

```sh
# 1. build everything
make build           # the chain binary  -> ./build/perpd
make build-sidecar   # the sidecar       -> ./oracle-sidecar/build/oracle-sidecar

# 2. start the sidecar
./oracle-sidecar/build/oracle-sidecar --config ./oracle-sidecar/oracle.json -v

# 3. (in another shell) initialise & run the chain
./build/perpd init mynode --chain-id perpdex-dev
./build/perpd genesis add-genesis-account ...
./build/perpd genesis gentx ...
./build/perpd genesis collect-gentxs

# 4. enable the oracle daemon by adding [oracle] to ~/.perpd/config/app.toml
cat >> ~/.perpd/config/app.toml <<'TOML'
[oracle]
enabled = true
sidecar_address = "localhost:8080"
fetch_interval = "500ms"
fetch_timeout = "200ms"
sidecar_decimals = 8
max_age = "5s"
TOML

# 5. start the chain
./build/perpd start
```

After ~30 seconds the chain's `OraclePrice` store will start receiving
non-zero entries; you can verify with:

```sh
perpd query oracle prices
```

## Reference docs

- [`sidecar-setup.md`](./sidecar-setup.md) — `oracle.json` schema, provider
  configuration, systemd unit template.
- [`deployment-docker.md`](./deployment-docker.md) — production-ready
  `docker-compose.yml` (chain + sidecar + Prometheus + Grafana).
- [`troubleshooting.md`](./troubleshooting.md) — common failure modes and
  resolutions.

## Design rationale

- **Why a sidecar?** Pulling from exchanges in-process would couple chain
  liveness to exchange uptime and add HTTPS keep-alives to the consensus
  goroutine. The split mirrors dydx v4 (`protocol/daemons/slinky`).
- **Why ExtendedCommitInfo (not an SDK Msg)?** Avoids ante-handler
  bypass tricks; PreBlock can re-derive the median independently of the
  proposer; the encoding is the same one cometbft itself signs over.
- **Why uint32 prices?** The chain's existing `OraclePrice` shape uses
  uint32. We scale sidecar prices into per-market integer encoding via
  the resolver's `Decimals` field. Migration to `uint64`/`math.Int` is a
  separate, dedicated PR.
