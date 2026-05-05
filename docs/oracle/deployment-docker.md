# Docker / Compose deployment

This is a single-host development stack that runs the chain, the
oracle-sidecar, Prometheus and Grafana side by side. It is the fastest
way to validate end-to-end oracle behaviour on a fresh machine.

## docker-compose.yml

```yaml
version: "3.9"

services:
  oracle-sidecar:
    image: ghcr.io/perpdex/perpdex-l1-oracle-sidecar:dev
    build:
      context: ../..
      dockerfile: oracle-sidecar/Dockerfile
    command: ["--config", "/etc/sidecar/oracle.json"]
    ports:
      - "8080:8080"   # gRPC
      - "8002:8002"   # /metrics + /healthz
    volumes:
      - ./oracle.json:/etc/sidecar/oracle.json:ro
    restart: unless-stopped

  perpd:
    image: ghcr.io/perpdex/perpdex-l1-perpd:dev
    build:
      context: ../..
      dockerfile: build/Dockerfile
    depends_on:
      - oracle-sidecar
    environment:
      - DAEMON_NAME=perpd
      - DAEMON_HOME=/root/.perpd
    volumes:
      - perpd_data:/root/.perpd
      - ./app.toml:/root/.perpd/config/app.toml:ro
    ports:
      - "26656:26656"
      - "26657:26657"
      - "1317:1317"
      - "9090:9090"
    restart: unless-stopped

  prometheus:
    image: prom/prometheus:v2.55.1
    ports:
      - "9091:9090"
    volumes:
      - ./prometheus.yml:/etc/prometheus/prometheus.yml:ro

  grafana:
    image: grafana/grafana:11.5.1
    depends_on:
      - prometheus
    ports:
      - "3000:3000"
    environment:
      - GF_AUTH_ANONYMOUS_ENABLED=true
      - GF_AUTH_ANONYMOUS_ORG_ROLE=Editor

volumes:
  perpd_data:
```

## Sidecar Dockerfile

```dockerfile
# oracle-sidecar/Dockerfile
FROM golang:1.25.7 AS builder
WORKDIR /src
COPY oracle-sidecar/ ./oracle-sidecar/
WORKDIR /src/oracle-sidecar
RUN CGO_ENABLED=0 go build -trimpath -o /oracle-sidecar ./cmd/oracle-sidecar

FROM gcr.io/distroless/static:nonroot
COPY --from=builder /oracle-sidecar /usr/local/bin/oracle-sidecar
USER 65532:65532
ENTRYPOINT ["/usr/local/bin/oracle-sidecar"]
```

## prometheus.yml

```yaml
global:
  scrape_interval: 5s
scrape_configs:
  - job_name: oracle-sidecar
    static_configs:
      - targets: ["oracle-sidecar:8002"]
  - job_name: perpd
    static_configs:
      - targets: ["perpd:26660"]   # cometbft default if enabled
```

## app.toml

```toml
[oracle]
enabled = true
sidecar_address = "oracle-sidecar:8080"
fetch_interval = "500ms"
fetch_timeout = "200ms"
sidecar_decimals = 8
max_age = "5s"
```

## Running

```sh
docker compose up --build
```

Confirm operation:

```sh
grpcurl -plaintext localhost:8080 perpdex.oracle.sidecar.v1.Oracle/Prices
curl http://localhost:9091/api/v1/query?query=perpdex_oracle_sidecar_pairs_observed
docker compose exec perpd perpd query oracle prices
```

## Production hardening checklist

- Restrict the sidecar's gRPC bind to `127.0.0.1:8080` and run the chain
  in the same network namespace; do NOT expose 8080 to the internet.
- Set CPU/RAM limits in the compose file.
- Forward `journalctl` / docker logs to your central log store.
- Wire alerts on:
  - `up{job="oracle-sidecar"} == 0`
  - `perpdex_oracle_sidecar_snapshot_age_seconds > 5`
  - `perpdex_oracle_sidecar_pairs_observed < <expected>`
- Pin sidecar / perpd image tags to immutable digests once you're past
  development.
