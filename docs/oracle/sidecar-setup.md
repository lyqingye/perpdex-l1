# Oracle sidecar setup

The sidecar is a single Go binary built from `./oracle-sidecar`. It reads a
JSON config and exposes both gRPC (price API consumed by the chain
daemon) and HTTP (Prometheus / health endpoints).

## Build

```sh
cd oracle-sidecar
make build         # writes ./build/oracle-sidecar
make test          # unit tests + bufconn-based gRPC e2e
make run           # local dev run with the bundled oracle.json
```

Or, from the repo root:

```sh
make build-sidecar
```

## oracle.json schema

```json
{
  "grpc_addr":     ":8080",
  "metrics_addr":  ":8002",
  "max_age":       "5s",
  "min_sources":   1,
  "pairs":         ["BTC/USD", "ETH/USD", "SOL/USD"],
  "providers": {
    "binance":   { "enabled": true,  "interval": "1500ms", "timeout": "1s",  "decimals": 8 },
    "okx":       { "enabled": true,  "interval": "1500ms", "timeout": "1s",  "decimals": 8 },
    "coingecko": {
      "enabled":  true,
      "interval": "5s",
      "timeout":  "3s",
      "decimals": 8,
      "api_key":  "<optional pro key>",
      "slugs": {
        "BTC": "bitcoin",
        "ETH": "ethereum",
        "SOL": "solana"
      }
    }
  }
}
```

### Top-level fields

| key             | default       | meaning                                                      |
|-----------------|---------------|--------------------------------------------------------------|
| `grpc_addr`     | `:8080`       | TCP bind address for the `Oracle.Prices` RPC                 |
| `metrics_addr`  | `:8002`       | TCP bind address for `/metrics` and `/healthz`               |
| `max_age`       | `5s`          | Drop a sample older than this from the cross-provider median |
| `min_sources`   | `1`           | Require at least N fresh providers before publishing a price |
| `pairs`         | required      | `BASE/QUOTE` strings — what to track                         |
| `providers`     | required      | per-provider knobs (see below)                               |

### Per-provider fields

| key       | applies to            | meaning                                                                |
|-----------|-----------------------|------------------------------------------------------------------------|
| `enabled` | all                   | turn the provider on/off without removing it from the file             |
| `interval`| all                   | poll cadence                                                            |
| `timeout` | all                   | per-request HTTP timeout                                               |
| `decimals`| all                   | integer-encoding decimals on the wire (default 8)                      |
| `api_key` | coingecko             | x-cg-pro-api-key header value                                          |
| `slugs`   | coingecko             | required mapping `BASE-symbol → coingecko-slug` (e.g. `BTC→bitcoin`)  |
| `endpoint`| all                   | override the default upstream URL (testing / private mirror)           |

## Running

```sh
oracle-sidecar --config /etc/perpdex/oracle.json -v
```

`-v` enables verbose startup logging including the full parsed config (do
not enable in production logs that are captured by an SIEM — the
`api_key` will be visible).

## systemd

```ini
# /etc/systemd/system/oracle-sidecar.service
[Unit]
Description=perpdex oracle sidecar
After=network-online.target
Wants=network-online.target

[Service]
Type=simple
User=perpdex
Group=perpdex
ExecStart=/usr/local/bin/oracle-sidecar --config /etc/perpdex/oracle.json
Restart=always
RestartSec=2
LimitNOFILE=65535
NoNewPrivileges=true
ProtectSystem=strict
ProtectHome=true
PrivateTmp=true
ReadWritePaths=/var/log/perpdex

[Install]
WantedBy=multi-user.target
```

```sh
systemctl daemon-reload
systemctl enable --now oracle-sidecar
journalctl -fu oracle-sidecar
```

## Health checks

```sh
curl -fsS http://localhost:8002/healthz   # {"ok"} if process is alive
grpcurl -plaintext localhost:8080 perpdex.oracle.sidecar.v1.Oracle/Prices
grpcurl -plaintext localhost:8080 perpdex.oracle.sidecar.v1.Oracle/Version
curl -fsS http://localhost:8002/metrics | grep perpdex_oracle_sidecar
```

The two key Prometheus metrics:

```
perpdex_oracle_sidecar_pairs_observed{} 3       # cardinality of latest snapshot
perpdex_oracle_sidecar_snapshot_age_seconds{} 0.42
```

Alert on `snapshot_age_seconds > 5` and `pairs_observed < pairs_count`.

## Adding a new market

1. Register the asset and market on the chain via the standard
   governance flow (`MsgRegisterAsset` / `MsgCreatePerpMarket`).
2. Add the `BASE/QUOTE` string to `pairs` in `oracle.json`.
3. If your base symbol is new, add the matching CoinGecko slug to
   `providers.coingecko.slugs`.
4. Restart the sidecar (`systemctl restart oracle-sidecar`). The chain
   daemon's resolver auto-refreshes every 10s — no chain restart needed.
